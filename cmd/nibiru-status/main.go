// Command nibiru-status prints the sakura status digest (nibiru#836) to stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/chenhg5/cc-connect/nibiru/status"
)

func main() {
	dir := flag.String("dir", status.DefaultDir(), "directory holding runner.log and workers.tsv")
	repo := flag.String("repo", "nibiru-labs/nibiru", "GitHub repo for merged PRs")
	events := flag.Int("events", 8, "recent runner events to show")
	flag.Parse()
	out := status.Generate(context.Background(), status.Live(*dir, *repo, nil), status.Options{MaxEvents: *events})
	fmt.Println(out)
	_ = os.Stdout.Sync()
}
