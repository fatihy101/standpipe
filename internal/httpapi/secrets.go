package httpapi

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// The known credential shapes.
var secretPrefixes = []string{
	"ghp_", "ghs_", "github_pat_", // GitHub
	"AKIA",       // AWS access key id
	"xoxb-",      // Slack
	"xoxa-",      //
	"xoxp-",      //
	"xoxr-",      //
	"xoxs-",      //
	"-----BEGIN", // PEM
}

// minEntropyLen and entropyThreshold are the secret scan's heuristic: length >= 20 and
// normalised Shannon entropy H/log2(n) > 0.9, where n is the string length.
const (
	minEntropyLen     = 20
	entropyThreshold  = 0.9
	reasonScanMaxRune = 4096
)

// scanSecrets is step 13, the secret scan.
//
// It returns the KEY that matched and never the value. The message goes into a
// 400 body and into the log, and a scanner that echoed the thing it found
// would publish the credential it was written to protect.
func scanSecrets(inputs map[string]string, reason string) (detail string, found bool) {
	// Deterministic order: two runs of the same request must produce the same
	// message, or the 400 body changes between retries for no reason.
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if looksLikeCredential(inputs[k]) {
			return fmt.Sprintf("input value resembles a credential (key: %s)", k), true
		}
	}
	if looksLikeCredential(reason) {
		return "reason resembles a credential", true
	}
	return "", false
}

func looksLikeCredential(v string) bool {
	if v == "" {
		return false
	}
	for _, p := range secretPrefixes {
		if strings.Contains(v, p) {
			return true
		}
	}
	if len(v) < minEntropyLen || len(v) > reasonScanMaxRune {
		return false
	}
	return normalisedEntropy(v) > entropyThreshold
}

// normalisedEntropy is the secret scan's H/log2(n).
//
// The denominator is log2 of the STRING LENGTH, as the design writes it — not log2
// of the alphabet size, which is the more usual formula. Using the usual one
// would change the threshold's meaning and either flood callers with 400s or
// stop catching anything, so the spec's form is kept literally.
func normalisedEntropy(s string) float64 {
	n := len(s)
	if n < 2 {
		return 0
	}
	var freq [256]int
	for i := 0; i < n; i++ {
		freq[s[i]]++
	}
	h := 0.0
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return h / math.Log2(float64(n))
}
