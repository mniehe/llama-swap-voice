// Command parse-duration compares the NixOS module's duration converter with
// time.ParseDuration over the rows nix/tests/duration-corpus.nix writes.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const refused = "refused"

type row struct {
	Input string `json:"input"`
	Nix   *int64 `json:"nix"`
}

func goResult(input string) string {
	d, err := time.ParseDuration(input)
	if err != nil {
		return refused
	}

	return strconv.FormatInt(int64(d), 10)
}

func nixResult(r row) string {
	if r.Nix == nil {
		return refused
	}

	return strconv.FormatInt(*r.Nix, 10)
}

// The module does not accept a sign, so a signed row must be refused even
// where Go reads it.
func want(input, fromGo string) string {
	if strings.HasPrefix(input, "+") || strings.HasPrefix(input, "-") {
		return refused
	}

	return fromGo
}

func main() {
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	var rows []row
	if err := json.Unmarshal(data, &rows); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	failed := 0
	for _, r := range rows {
		fromGo := goResult(r.Input)
		fromNix := nixResult(r)
		status := "ok"
		if fromNix != want(r.Input, fromGo) {
			status = "MISMATCH"
			failed++
		}
		fmt.Printf("%-8s %-30q nix=%-20s go=%s\n", status, r.Input, fromNix, fromGo)
	}

	if failed > 0 {
		fmt.Fprintf(os.Stderr, "%d of %d durations differ from time.ParseDuration\n", failed, len(rows))
		os.Exit(1)
	}

	fmt.Printf("%d durations agree with time.ParseDuration\n", len(rows))
}
