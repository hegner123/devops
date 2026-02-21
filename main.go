//go:build !agent

package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(Version)
		return
	}

	st, err := newStore(dbPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer st.close()

	pool, err := newConnPool()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize SSH pool: %v\n", err)
		os.Exit(1)
	}

	s := &server{
		store: st,
		pool:  pool,
		agent: &agentClient{pool: pool},
	}
	s.run()
}
