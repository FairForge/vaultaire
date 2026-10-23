// Command leakage measures what a Private space's sealed chunk LENGTHS tell
// the server (Prompt 31 step 4.5, decision F-35). It is a measurement, not
// part of the SDK: testdata/ keeps it out of ./... builds.
//
//	go run ./sdk/go/e2ee/testdata/leakage -list files.txt [-label name]
//
// files.txt holds one path per line. For each file it cuts the chunks the
// frozen chunker cuts (WP-R8-4: restic Rabin, polynomial 0x2ADD89E3B790BB,
// 1 MiB min, 20 average bits, 16 MiB max — the constants of
// internal/crypto/chunker.go, restated), seals nothing (a sealed chunk is
// len + 41 bytes in every space: e2ee.ChunkOverhead), and reports for three
// length policies — none (today), Padmé, power-of-two buckets:
//
//   - the stored overhead (total, and the worst chunk's padding over its
//     len + 41);
//   - the distinct stored lengths seen and how many files share their
//     sequence with another file of the corpus;
//   - the collision entropy H2 per chunk of the chunker's length
//     distribution under the policy (the model: min + geometric with mean
//     2^20, cut at max — what restic's split mask gives), and from it the
//     bits a file's sequence leaks; the share of files whose sequence
//     carries ≥ 40 bits (unique among ~10^12 objects);
//   - the anonymity set in the corpus itself: how many files (this one
//     included) show the same stored sequence — median over every file, and
//     over the single-chunk files, whose one length is their stored size.
//
// With -compare-pol it also cuts every file under a second polynomial (as a
// per-space secret polynomial would — restic's own defence: a random
// polynomial per repository) and counts the files whose stored sequence is
// the same under both, and the chunk lengths the two cuts share.
//
// Only aggregates are printed: never a path.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/restic/chunker"
)

const (
	pol      = chunker.Pol(0x2ADD89E3B790BB)
	minSize  = 1 << 20
	maxSize  = 16 << 20
	avgBits  = 20
	overhead = 41 // e2ee.ChunkOverhead
)

// padme is Nikitin et al., "Reducing Metadata Leakage from Encrypted Files
// and Communication with PURBs" (PoPETs 2019): keep the top
// floor(log2 E)+1 bits of the length's exponent E, round the rest up.
func padme(l uint64) uint64 {
	if l < 2 {
		return l
	}
	e := uint64(63 - bits.LeadingZeros64(l))
	s := uint64(64 - bits.LeadingZeros64(e)) // floor(log2 e) + 1
	last := e - s
	mask := uint64(1)<<last - 1
	return (l + mask) &^ mask
}

func pow2(l uint64) uint64 {
	if l <= 1 {
		return l
	}
	return 1 << (64 - bits.LeadingZeros64(l-1))
}

type policy struct {
	name string
	pad  func(uint64) uint64
}

var policies = []policy{
	{"none (len+41)", func(l uint64) uint64 { return l + overhead }},
	{"Padmé", func(l uint64) uint64 { return padme(l + overhead) }},
	{"power-of-two", func(l uint64) uint64 { return pow2(l + overhead) }},
}

// modelH2 is −log2 Σ p² of the stored length of a non-final chunk under a
// policy, p from the model: length = min + k, k geometric with success
// probability 2^-20 per byte, the mass past max put on max.
func modelH2(pad func(uint64) uint64) float64 {
	q := 1 - math.Pow(2, -avgBits)
	mass := map[uint64]float64{}
	p := 1 - q // P(k = 0)
	var used float64
	for l := uint64(minSize); l < maxSize; l++ {
		mass[pad(l)] += p
		used += p
		p *= q
	}
	mass[pad(maxSize)] += 1 - used
	var s float64
	for _, m := range mass {
		s += m * m
	}
	return -math.Log2(s)
}

func main() {
	list := flag.String("list", "", "file with one path per line")
	label := flag.String("label", "corpus", "name printed with the results")
	comparePol := flag.Uint64("compare-pol", 0, "a second irreducible polynomial to cut every file with (0 = off)")
	flag.Parse()
	f, err := os.Open(*list)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	var files [][]uint64 // chunk lengths per file
	var compared, sameSeq, sameMulti, sharedLens, totalLens int
	var bytesTotal uint64
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	buf := make([]byte, maxSize)
	for sc.Scan() {
		path := strings.TrimSpace(sc.Text())
		if path == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(path); err == nil {
			path = real
		}
		if seen[path] {
			continue // one file listed twice is not two files
		}
		seen[path] = true
		in, err := os.Open(path) // #nosec G304 -- a measurement over files the operator lists
		if err != nil {
			continue
		}
		lens := cut(in, pol, buf)
		_ = in.Close()
		for _, n := range lens {
			bytesTotal += n
		}
		if len(lens) == 0 {
			continue
		}
		files = append(files, lens)
		if *comparePol != 0 {
			in2, err := os.Open(path) // #nosec G304 -- as above
			if err != nil {
				continue
			}
			other := cut(in2, chunker.Pol(*comparePol), buf)
			_ = in2.Close()
			compared++
			if fmt.Sprint(other) == fmt.Sprint(lens) {
				sameSeq++
				if len(lens) > 1 {
					sameMulti++
				}
			}
			set := map[uint64]int{}
			for _, n := range lens {
				set[n]++
			}
			for _, n := range other {
				if set[n] > 0 {
					set[n]--
					sharedLens++
				}
			}
			totalLens += len(lens)
		}
	}

	multi := 0
	chunks := 0
	for _, l := range files {
		chunks += len(l)
		if len(l) > 1 {
			multi++
		}
	}
	fmt.Printf("## %s: %d files, %d chunks, %.1f MiB; %d files of ≥ 2 chunks\n\n", *label, len(files), chunks, float64(bytesTotal)/(1<<20), multi)
	fmt.Println("| policy | stored overhead | worst padding | model H2 / non-final chunk | distinct stored lengths | files sharing their sequence | anonymity set, median (all / single-chunk) | multi-chunk files ≥ 40 bits | median bits, multi-chunk |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|")
	for _, p := range policies {
		var raw, stored uint64
		worst := 0.0
		distinct := map[uint64]bool{}
		seqs := map[string]int{}
		for _, l := range files {
			var key strings.Builder
			for _, n := range l {
				s := p.pad(n)
				raw += n
				stored += s
				if o := float64(s-n-overhead) / float64(n+overhead); o > worst {
					worst = o
				}
				distinct[s] = true
				fmt.Fprintf(&key, "%d,", s)
			}
			seqs[key.String()]++
		}
		shared := 0
		for _, n := range seqs {
			if n > 1 {
				shared += n
			}
		}
		var setAll, setOne []int
		for _, l := range files {
			var key strings.Builder
			for _, n := range l {
				fmt.Fprintf(&key, "%d,", p.pad(n))
			}
			n := seqs[key.String()]
			setAll = append(setAll, n)
			if len(l) == 1 {
				setOne = append(setOne, n)
			}
		}
		h2 := modelH2(p.pad)
		var leaks []float64
		over40 := 0
		for _, l := range files {
			if len(l) < 2 {
				continue
			}
			// Every chunk but the last follows the model; the last is a
			// remainder, counted at the same H2 (a lower bound for none).
			b := float64(len(l)) * h2
			leaks = append(leaks, b)
			if b >= 40 {
				over40++
			}
		}
		sort.Float64s(leaks)
		median := 0.0
		if len(leaks) > 0 {
			median = leaks[len(leaks)/2]
		}
		pct := 0.0
		if multi > 0 {
			pct = 100 * float64(over40) / float64(multi)
		}
		fmt.Printf("| %s | %.2f %% | %.1f %% | %.1f bits | %d | %d | %d / %d | %.0f %% | %.0f |\n", p.name,
			100*float64(stored-raw)/float64(raw), 100*worst, h2, len(distinct), shared, medianInt(setAll), medianInt(setOne), pct, median)
	}
	fmt.Println()
	if *comparePol != 0 {
		ok := chunker.Pol(*comparePol).Irreducible()
		fmt.Printf("Second polynomial %#x (irreducible: %v): %d files compared; %d keep the same sequence (%d of them multi-chunk — the rest are single-chunk files, whose one length is their size); %d of %d chunk lengths coincide.\n\n",
			*comparePol, ok, compared, sameSeq, sameMulti, sharedLens, totalLens)
	}
}

func cut(r io.Reader, p chunker.Pol, buf []byte) []uint64 {
	c := chunker.NewWithBoundaries(r, p, minSize, maxSize)
	c.SetAverageBits(avgBits)
	var lens []uint64
	for {
		ch, err := c.Next(buf)
		if err == io.EOF {
			return lens
		}
		if err != nil {
			return nil
		}
		lens = append(lens, uint64(ch.Length))
	}
}

func medianInt(v []int) int {
	if len(v) == 0 {
		return 0
	}
	sort.Ints(v)
	return v[len(v)/2]
}
