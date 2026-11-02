package instabox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// agentUnit is the systemd service of the runner snapshot: it waits for the
// configs the provider writes after the machine is created (one file per slot,
// see dispatch.SlotConfig) and runs the job each names, until the machine is
// deleted. A job that arrives through Provider.Run uses slot 0.
const agentUnit = `[Unit]
Description=Raccoon CI agent: runs the jobs a controller assigns
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=user
WorkingDirectory=/data/home
# The sources of the jobs are unpacked under TMPDIR: /tmp is a small memory
# disk, and a machine may hold several jobs' sources at once.
Environment=TMPDIR=/data/home/tmp
ExecStartPre=/bin/mkdir -p /data/home/tmp
ExecStart=/usr/local/bin/raccoonci agent -loop -slots 8 -wait 24h -config /data/home/.raccoonci-agent.json
Restart=no

[Install]
WantedBy=multi-user.target
`

// RunnerSpec describes the runner snapshot to build.
type RunnerSpec struct {
	// Binary is the raccoonci executable to install. It must be statically
	// linked (CGO_ENABLED=0) so that it runs on whatever template is used.
	Binary []byte
	// Version is what `raccoonci -version` prints for Binary; it is checked on
	// the machine and recorded in the manifest.
	Version string
	// Template is the template id to start from, default "universal".
	Template string
	// Name is the snapshot's name (lowercase letters, digits and dashes).
	Name string
}

// Machine is what we read of a machine.
type Machine struct {
	ID       string `json:"id"`
	Template struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	} `json:"template"`
}

// Machine returns one machine.
func (c *Client) Machine(ctx context.Context, id string) (Machine, error) {
	var m Machine
	err := c.do(ctx, "GET", "/v1/machines/"+url.PathEscape(id), nil, "", nil, &m)
	return m, err
}

// BuildRunnerSnapshot makes the snapshot machines are created from for
// Provider: a clean machine of the template with raccoonci installed in
// /usr/local/bin and the agent service enabled. Nothing else goes in: no
// toolchains, dependencies, secrets or sign-ins. The machine used is deleted
// whatever happens, and a snapshot that did not become ready is deleted too.
func (c *Client) BuildRunnerSnapshot(ctx context.Context, spec RunnerSpec, logf func(string, ...any)) (snapshotID string, err error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if len(spec.Binary) == 0 || spec.Version == "" || spec.Name == "" {
		return "", errors.New("instabox: a runner snapshot needs a binary, its version and a name")
	}
	template := spec.Template
	if template == "" {
		template = "universal"
	}
	var b [3]byte
	rand.Read(b[:])
	machine, err := c.CreateMachine(ctx, CreateRequest{
		Name:               "rc-build-" + hex.EncodeToString(b[:]),
		TemplateID:         template,
		DeleteAfterSeconds: 3600, // the server deletes it even if this process dies
	})
	if machine != "" {
		defer func() {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			if derr := c.DeleteMachine(dctx, machine); derr != nil {
				logf("could not delete machine %s (the server deletes it within an hour): %v", machine, derr)
			}
		}()
	}
	if err != nil {
		return "", fmt.Errorf("creating the build machine: %w", err)
	}
	logf("build machine %s is up", machine)

	m, err := c.Machine(ctx, machine)
	if err != nil {
		return "", err
	}
	manifest, _ := json.Marshal(map[string]string{
		"raccoonci": spec.Version,
		"template":  strings.TrimSpace(m.Template.ID + " " + m.Template.Version),
		"agent":     "systemd raccoonci-agent.service, config /data/home/.raccoonci-agent.json; commands run, run-instance, agent",
	})

	for path, data := range map[string][]byte{
		"/tmp/raccoonci-new":           spec.Binary,
		"/tmp/raccoonci-agent.service": []byte(agentUnit),
		"/tmp/raccoonci-runner.json":   manifest,
	} {
		mode := "0644"
		if path == "/tmp/raccoonci-new" {
			mode = "0755"
		}
		if err := c.PutFile(ctx, machine, path, mode, data); err != nil {
			return "", fmt.Errorf("uploading %s: %w", path, err)
		}
	}
	sh := func(what, cmd string) (string, error) {
		code, out, err := c.Exec(ctx, machine, cmd)
		if err != nil {
			return out, fmt.Errorf("%s: %w", what, err)
		}
		if code != 0 {
			return out, fmt.Errorf("%s: exit %d: %s", what, code, strings.TrimSpace(out))
		}
		return out, nil
	}

	out, err := sh("running the uploaded binary", "/tmp/raccoonci-new -version")
	if err != nil {
		return "", err
	}
	if got := strings.TrimSpace(out); got != spec.Version {
		return "", fmt.Errorf("the uploaded binary reports version %q, expected %q", got, spec.Version)
	}
	// ldd exits 1 and says so for a static executable.
	if out, _ := sh("checking linkage", "ldd /tmp/raccoonci-new 2>&1; true"); !strings.Contains(out, "not a dynamic executable") && !strings.Contains(out, "statically linked") {
		return "", errors.New("the binary is dynamically linked; build it with CGO_ENABLED=0 so it runs on any template")
	}
	if _, err := sh("installing", "sudo install -m 0755 /tmp/raccoonci-new /usr/local/bin/raccoonci && "+
		"sudo install -m 0644 /tmp/raccoonci-agent.service /etc/systemd/system/raccoonci-agent.service && "+
		"sudo install -m 0644 /tmp/raccoonci-runner.json /etc/raccoonci-runner.json && "+
		"sudo systemctl daemon-reload && sudo systemctl enable raccoonci-agent && "+
		"rm -f /tmp/raccoonci-new /tmp/raccoonci-agent.service /tmp/raccoonci-runner.json"); err != nil {
		return "", err
	}
	// A snapshot copies the disks: no run's token may be on them.
	if _, err := sh("checking the machine is clean", "test ! -e /data/home/.raccoonci-agent.json && /usr/local/bin/raccoonci -version"); err != nil {
		return "", err
	}

	logf("saving the snapshot %s", spec.Name)
	id, err := c.Snapshot(ctx, machine, spec.Name, "Raccoon CI runner: "+template+" + raccoonci "+spec.Version+", systemd agent service")
	if err != nil {
		if id != "" {
			c.DeleteSnapshot(context.WithoutCancel(ctx), id)
		}
		return "", err
	}
	return id, nil
}
