// Command affected prints, as JSON, the Deployables of a Project that are
// affected between two git refs. See the repository README for the contract.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/worldtree-industries/workflows/internal/affected"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := affected.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
