package secrets

// `bashy secret gen` — mint a fresh random password locally.
//
// This is the one subcommand of `secret` that never talks to the vault: no
// token, no network, no cache. It prints and exits, so the natural way to
// use it is the pipe the vault already understands:
//
//	bashy secret gen | bashy secret set DB_PASSWORD
//
// Algorithm of record: 1Password's Strong Password Generator
// (github.com/1Password/spg, char_gen.go), re-implemented over the standard
// library so this package stays stdlib + cobra:
//
//   - the alphabet is the union of the enabled character CLASSES (lower,
//     upper, digits, symbols), deduplicated, minus the --no-ambiguous set;
//   - every enabled class is REQUIRED to appear at least once, enforced by
//     rejection sampling: draw a uniform string over the whole alphabet,
//     accept iff every class is present, else redraw. Unlike the common
//     "pick one per class, then shuffle" trick this keeps the output exactly
//     uniform over the set of strings that satisfy the policy;
//   - the redraw loop is bounded (genMaxTrials) and a recipe whose acceptance
//     probability makes that bound reachable is refused up front, so the
//     loop cannot spin and an exhausted loop is a real error, not a retry;
//   - entropy is the exact log2 of the number of acceptable strings
//     (inclusion–exclusion over the required classes, in math/big), not the
//     optimistic length·log2(alphabet).
//
// Character indices come from crypto/rand.Int (the idiom go-password uses),
// which rejection-samples internally — no `byte % n` modulo bias. Nothing
// here imports math/rand; a test enforces that.
//
// Not offered on purpose: pronounceable passwords (structured, weaker per
// character) and passphrases (a wordlist is a licensing decision of its own).

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

const (
	genSchemaVersion = "bashy-secret-gen-v1"

	genLower  = "abcdefghijklmnopqrstuvwxyz"
	genUpper  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	genDigits = "0123456789"
	// No quote, backslash, backtick, space, <>|/~: the result pastes into an
	// env file, YAML, JSON and a single-quoted shell string without escaping.
	genSymbols = "!@#$%^&*()-_=+[]{}:;,.?"
	// pwgen's -B set: glyphs that read alike in common fonts.
	genAmbiguous = "B8G6I1l0OQDS5Z2"

	genDefaultLength = 20
	genMaxLength     = 1024
	genMaxCount      = 10000

	// genMaxTrials bounds the rejection loop. genMaxFailRate is the largest
	// probability of exhausting it that a recipe may have: a recipe with a
	// worse acceptance rate is refused before the first draw. 2^-40 keeps the
	// exhausted-loop error effectively unreachable for any accepted recipe.
	genMaxTrials   = 1000
	genMaxFailRate = 1.0 / (1 << 40)
)

// randReader is the entropy source. It is a variable only so a test can
// count reads; nothing else may assign it.
var randReader io.Reader = rand.Reader

// randIndex returns a uniform integer in [0, n).
func randIndex(n int) (int, error) {
	v, err := rand.Int(randReader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(v.Int64()), nil
}

// genRecipe is the resolved policy: the alphabet to draw from and the
// classes that must each appear at least once (empty for --charset).
type genRecipe struct {
	length   int
	alphabet []rune
	required [][]rune
}

type genOptions struct {
	length      int
	count       int
	noLower     bool
	noUpper     bool
	noDigits    bool
	noSymbols   bool
	symbols     string
	charset     string
	noAmbiguous bool
	jsonOut     bool
	verbose     bool
}

func newGenCmd() *cobra.Command {
	var o genOptions
	cmd := &cobra.Command{
		Use:   "gen [LENGTH]",
		Short: "Generate a random password locally (crypto/rand; never touches the vault)",
		Long: `Generate one or more random passwords and print them, one per line.
This runs entirely on this machine: no token, no network, no cache.

By default a password is 20 characters drawn uniformly from lower- and
upper-case letters, digits and the symbols ` + genSymbols + `, and is
guaranteed to contain at least one of each class (drawn by rejection so the
result stays uniform over every password that satisfies the policy).
--charset replaces the classes with an explicit alphabet and drops the
per-class guarantee. --json reports the exact entropy in bits.

Store the result without it ever appearing in shell history or an agent's
transcript:

  bashy secret gen | bashy secret set DB_PASSWORD
  bashy secret gen --charset 0123456789abcdef -l 64   # a hex token`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 1 {
				if c.Flags().Changed("length") {
					return errors.New("gen: length given both as LENGTH and --length")
				}
				n, err := strconv.Atoi(args[0])
				if err != nil {
					return fmt.Errorf("gen: invalid LENGTH %q", args[0])
				}
				o.length = n
			}
			return runGen(c.OutOrStdout(), c.ErrOrStderr(), o)
		},
	}
	f := cmd.Flags()
	f.IntVarP(&o.length, "length", "l", genDefaultLength, "password length in characters")
	f.IntVarP(&o.count, "count", "n", 1, "how many passwords to print, one per line")
	f.BoolVar(&o.noLower, "no-lower", false, "omit lower-case letters")
	f.BoolVar(&o.noUpper, "no-upper", false, "omit upper-case letters")
	f.BoolVar(&o.noDigits, "no-digits", false, "omit digits")
	f.BoolVar(&o.noSymbols, "no-symbols", false, "omit symbols")
	f.StringVar(&o.symbols, "symbols", genSymbols, "the symbol class")
	f.StringVar(&o.charset, "charset", "", "draw from exactly these characters (no per-class guarantee)")
	f.BoolVarP(&o.noAmbiguous, "no-ambiguous", "B", false, "exclude look-alike characters ("+genAmbiguous+")")
	f.BoolVar(&o.jsonOut, "json", false, "write a JSON envelope with the passwords and their entropy")
	f.BoolVarP(&o.verbose, "verbose", "v", false, "report alphabet size and entropy on stderr")
	return cmd
}

func runGen(out, errOut io.Writer, o genOptions) error {
	if o.count < 1 || o.count > genMaxCount {
		return fmt.Errorf("gen: count must be between 1 and %d", genMaxCount)
	}
	r, err := o.recipe()
	if err != nil {
		return err
	}
	bits := r.entropyBits()
	if p := r.acceptRate(); math.Pow(1-p, genMaxTrials) > genMaxFailRate {
		if p == 0 {
			return fmt.Errorf("gen: a %d-character password cannot contain one of each of %d classes", r.length, len(r.required))
		}
		return fmt.Errorf("gen: length %d is too short to reliably contain one of each of %d classes (acceptance rate %.1f%%); use a longer length or drop a class", r.length, len(r.required), p*100)
	}

	passwords := make([]string, 0, o.count)
	for range o.count {
		pw, err := r.generate()
		if err != nil {
			return fmt.Errorf("gen: %w", err)
		}
		passwords = append(passwords, pw)
	}

	if o.verbose {
		fmt.Fprintf(errOut, "gen: %d characters from an alphabet of %d, %.1f bits of entropy\n", r.length, len(r.alphabet), bits)
	}
	if o.jsonOut {
		env := map[string]any{
			"schema_version": genSchemaVersion,
			"length":         r.length,
			"count":          o.count,
			"alphabet_size":  len(r.alphabet),
			"entropy_bits":   math.Round(bits*100) / 100,
			"passwords":      passwords,
		}
		return json.NewEncoder(out).Encode(env)
	}
	for _, pw := range passwords {
		fmt.Fprintln(out, pw)
	}
	return nil
}

// recipe resolves the options into the alphabet and required classes,
// following spg's Allow → Exclude → Require order.
func (o genOptions) recipe() (*genRecipe, error) {
	if o.length < 1 || o.length > genMaxLength {
		return nil, fmt.Errorf("gen: length must be between 1 and %d", genMaxLength)
	}
	r := &genRecipe{length: o.length}

	if o.charset != "" {
		r.alphabet = runeSet(o.charset, o.noAmbiguous)
		if len(r.alphabet) < 2 {
			return nil, errors.New("gen: --charset needs at least two distinct characters")
		}
		return r, nil
	}

	type class struct {
		name    string
		chars   string
		enabled bool
	}
	classes := []class{
		{"lower-case letters", genLower, !o.noLower},
		{"upper-case letters", genUpper, !o.noUpper},
		{"digits", genDigits, !o.noDigits},
		{"symbols", o.symbols, !o.noSymbols},
	}
	union := map[rune]bool{}
	for _, c := range classes {
		if !c.enabled {
			continue
		}
		set := runeSet(c.chars, o.noAmbiguous)
		if len(set) == 0 {
			return nil, fmt.Errorf("gen: the %s class is empty", c.name)
		}
		r.required = append(r.required, set)
		for _, ch := range set {
			union[ch] = true
		}
	}
	if len(r.required) == 0 {
		return nil, errors.New("gen: every character class is disabled")
	}
	for ch := range union {
		r.alphabet = append(r.alphabet, ch)
	}
	slices.Sort(r.alphabet)
	if len(r.alphabet) < 2 {
		return nil, errors.New("gen: the alphabet needs at least two distinct characters")
	}
	return r, nil
}

// runeSet deduplicates and sorts s, dropping the ambiguous glyphs when asked.
func runeSet(s string, noAmbiguous bool) []rune {
	seen := map[rune]bool{}
	var out []rune
	for _, ch := range s {
		if seen[ch] || (noAmbiguous && strings.ContainsRune(genAmbiguous, ch)) {
			continue
		}
		seen[ch] = true
		out = append(out, ch)
	}
	slices.Sort(out)
	return out
}

// generate draws one password: uniform over the alphabet, redrawn until
// every required class is present.
func (r *genRecipe) generate() (string, error) {
	buf := make([]rune, r.length)
	for range genMaxTrials {
		for i := range buf {
			k, err := randIndex(len(r.alphabet))
			if err != nil {
				return "", err
			}
			buf[i] = r.alphabet[k]
		}
		if r.satisfies(buf) {
			return string(buf), nil
		}
	}
	return "", fmt.Errorf("no acceptable password in %d draws", genMaxTrials)
}

func (r *genRecipe) satisfies(pw []rune) bool {
	for _, class := range r.required {
		found := false
		for _, ch := range pw {
			if containsRune(class, ch) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func containsRune(sorted []rune, ch rune) bool {
	_, found := slices.BinarySearch(sorted, ch)
	return found
}

// acceptable counts the strings of the recipe's length over its alphabet
// that contain at least one character of every required class:
// inclusion–exclusion over the subsets of classes that are MISSING.
func (r *genRecipe) acceptable() *big.Int {
	a := int64(len(r.alphabet))
	l := int64(r.length)
	total := new(big.Int)
	for mask := 0; mask < 1<<len(r.required); mask++ {
		missing := map[rune]bool{}
		bitsSet := 0
		for i, class := range r.required {
			if mask&(1<<i) == 0 {
				continue
			}
			bitsSet++
			for _, ch := range class {
				missing[ch] = true
			}
		}
		term := new(big.Int).Exp(big.NewInt(a-int64(len(missing))), big.NewInt(l), nil)
		if bitsSet%2 == 1 {
			total.Sub(total, term)
		} else {
			total.Add(total, term)
		}
	}
	return total
}

func (r *genRecipe) space() *big.Int {
	return new(big.Int).Exp(big.NewInt(int64(len(r.alphabet))), big.NewInt(int64(r.length)), nil)
}

// acceptRate is the probability one uniform draw satisfies the policy.
func (r *genRecipe) acceptRate() float64 {
	if len(r.required) == 0 {
		return 1
	}
	q := new(big.Float).Quo(new(big.Float).SetInt(r.acceptable()), new(big.Float).SetInt(r.space()))
	p, _ := q.Float64()
	return p
}

// entropyBits is log2 of the number of acceptable strings.
func (r *genRecipe) entropyBits() float64 {
	n := r.acceptable()
	if n.Sign() <= 0 {
		return 0
	}
	mant := new(big.Float)
	exp := new(big.Float).SetInt(n).MantExp(mant)
	m, _ := mant.Float64() // in [0.5, 1)
	return float64(exp) + math.Log2(m)
}
