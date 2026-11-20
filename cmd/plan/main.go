// Command plan prints, as GitHub Actions step outputs, what a Project's images
// job compares against, which image tag it builds, whether it pushes and which
// release it publishes. See the repository README for the contract.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/worldtree-industries/workflows/internal/plan"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := plan.Main(ctx, os.Args[1:], os.Stdout, os.Stderr, plan.GitHubLabels)
	stop()
	os.Exit(code)
}
