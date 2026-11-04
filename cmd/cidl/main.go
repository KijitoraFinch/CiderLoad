package main

import (
	"fmt"
	"os"
	"syscall"

	"ciderload/internal/launcher"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--cidl-help" {
		fmt.Print(launcher.Help)
		return
	}
	plan, err := launcher.Prepare(os.Args[1:], os.Environ())
	if err == nil {
		err = syscall.Exec(plan.Path, append([]string{plan.Path}, os.Args[1:]...), plan.Env)
	}
	fmt.Fprintln(os.Stderr, "cidl:", err)
	os.Exit(2)
}
