// Command genfixtures generates (or verifies the cache of) the predefined
// benchmark fixture trees and prints their sizes.
package main

import (
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/deppess/sfync/testenv/fixtures"
)

func main() {
	names := os.Args[1:]
	if len(names) == 0 {
		for n := range fixtures.Specs {
			names = append(names, n)
		}
		slices.Sort(names)
	}
	for _, n := range names {
		spec, ok := fixtures.Specs[n]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown fixture %q\n", n)
			os.Exit(2)
		}
		start := time.Now()
		dir, st, err := fixtures.Get(spec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", n, err)
			os.Exit(1)
		}
		fmt.Printf("%-8s %5d files %4d dirs %7.1f MB  (%v)  %s\n",
			n, st.Files, st.Dirs, float64(st.Bytes)/1e6, time.Since(start).Round(time.Millisecond), dir)
	}
}
