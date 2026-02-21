//go:build !agent

package main

import (
	"fmt"
	"os"
)

func main() {
	st, err := newStore(dbPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer st.close()

	s := &server{
		store: st,
	}
	s.run()
}
