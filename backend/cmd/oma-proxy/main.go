package main

import (
	"log"

	"agent-workbench/backend/internal/execution"
)

func main() {
	if err := execution.ServeProxyFromEnv(); err != nil {
		log.Fatal(err)
	}
}
