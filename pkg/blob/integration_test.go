package blob

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// TestRealS3 runs against a real S3-compatible server. It is skipped unless
// RACCOONCI_TEST_S3_ENDPOINT is set; the access key and secret come from
// RACCOONCI_TEST_S3_KEY and RACCOONCI_TEST_S3_SECRET.
func TestRealS3(t *testing.T) {
	endpoint := os.Getenv("RACCOONCI_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("RACCOONCI_TEST_S3_ENDPOINT not set")
	}
	s, err := NewS3(S3Config{
		Endpoint: endpoint, Bucket: fmt.Sprintf("raccoon-test-%d", time.Now().UnixNano()),
		AccessKey: os.Getenv("RACCOONCI_TEST_S3_KEY"), SecretKey: os.Getenv("RACCOONCI_TEST_S3_SECRET"),
		PathStyle: true, Prefix: "art/",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &s.cfg.Bucket}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	key := "run 1/job#2/名字 (v4).zip"
	chunks := [][]byte{make([]byte, MinPartSize), make([]byte, MinPartSize), make([]byte, 12345)}
	for _, c := range chunks {
		rand.Read(c)
	}
	up, err := s.BeginUpload(ctx, key, "application/zip")
	if err != nil {
		t.Fatal(err)
	}
	parts := make([]Part, len(chunks))
	var wg sync.WaitGroup
	for i := len(chunks) - 1; i >= 0; i-- {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A reader that is not seekable, like a request body.
			r := io.MultiReader(bytes.NewReader(chunks[i]))
			p, err := up.PutPart(ctx, i+1, r, int64(len(chunks[i])))
			if err != nil {
				t.Error(err)
			}
			parts[i] = p
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	// A block that is sent but never committed.
	if _, err := up.PutPart(ctx, 4, bytes.NewReader([]byte("stray")), 5); err != nil {
		t.Fatal(err)
	}
	if err := up.Complete(ctx, parts); err != nil {
		t.Fatal(err)
	}

	want := bytes.Join(chunks, nil)
	info, err := s.Stat(ctx, key)
	if err != nil || info.Size != int64(len(want)) {
		t.Fatalf("Stat = %+v, %v (want %d bytes)", info, err, len(want))
	}
	u, err := s.PresignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, want) {
		t.Fatalf("presigned GET = %d, %d bytes (want %d, identical)", resp.StatusCode, len(got), len(want))
	}

	up2, err := s.BeginUpload(ctx, "aborted", "")
	if err != nil {
		t.Fatal(err)
	}
	up2.PutPart(ctx, 1, bytes.NewReader([]byte("x")), 1)
	if err := up2.Abort(ctx); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat after Delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}
