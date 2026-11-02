package jobgraph

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/Raccoon-CI/raccoonci/pkg/runevent"
	"github.com/Raccoon-CI/raccoonci/pkg/value"
	"github.com/Raccoon-CI/raccoonci/pkg/workflow"
)

func TestGraphHandsResolvedInstancesToRemote(t *testing.T) {
	w, err := workflow.Parse([]byte(`
on: push
jobs:
  gen:
    runs-on: ubuntu-latest
    steps: [{run: echo}]
  build:
    needs: gen
    if: needs.gen.result == 'success'
    runs-on: ubuntu-latest
    strategy:
      matrix:
        n: ${{ fromJSON(needs.gen.outputs.list) }}
    steps: [{run: echo}]
`))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var names []string
	res, err := Run(context.Background(), w, Options{
		Remote: func(_ context.Context, inst RemoteInstance) (runevent.Result, error) {
			mu.Lock()
			names = append(names, inst.Name)
			mu.Unlock()
			r := runevent.Result{Conclusion: runevent.Success}
			if inst.Job.ID == "gen" {
				r.Outputs = value.Object{{Key: "list", Value: "[1,2]"}}
			}
			return r, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Conclusion != runevent.Success {
		t.Errorf("conclusion = %s", res.Conclusion)
	}
	if got := fmt.Sprint(len(names)); got != "3" {
		t.Errorf("instances = %v", names)
	}
}
