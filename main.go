package main

import (
	"ccoff-abs-bot/internal/app"
	"ccoff-abs-bot/internal/operations"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "test-bot" {
		app.RunTest()
		return
	}
	if handled, err := operations.RunCLI(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	app.Run()
}
