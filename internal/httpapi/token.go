package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"math/big"
	"strconv"
)

const base62alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// base62Width is the number of characters 32 bytes occupy in base62:
// ceil(256 / log2(62)) = 43. The format fixes the field at EXACTLY 43, so a value
// with leading zero bytes is LEFT-PADDED, never trimmed — padding keeps the
// width fixed, trimming would mint a token both the published regex and this
// package's own parser reject.
const base62Width = 43

// MintToken creates a caller token in the format tokenScanRE matches and
// returns it with the
// SHA-256 hash to be written into standpipe.yaml.
//
// The plaintext is returned exactly once and stored nowhere. It lives
// here rather than in the CLI so that minting and validating a token are the
// same file: a format change that breaks one breaks the other in the same
// review.
func MintToken(tokenID string) (token, hash string, err error) {
	if !tokenIDRE.MatchString(tokenID) {
		return "", "", fmt.Errorf("token id %q must match %s", tokenID, tokenIDRE)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	secret := base62Encode(raw[:])
	stem := tokenPrefix + "_" + tokenID + "_" + secret
	sum := crc32.ChecksumIEEE([]byte(stem))
	token = stem + "_" + strconv.FormatUint(uint64(sum), 36)

	// The MINT-TIME SELF-CHECK, which the design makes normative: before a
	// token is emitted it goes through (a) the authentication parser and (b) the PUBLISHED
	// scanning regex, and either rejecting means no token is printed at all.
	//
	// This is the mechanism that catches a divergence between generator and
	// scanner — the prose does not. Both checks must therefore run against the
	// published pattern rather than a local, more forgiving copy: a private
	// regex would pass here and still leave a leaked token invisible to the
	// scanner it was written for, which is exactly the defect this guards. The match is
	// on the WHOLE token, because a partial hit is a finding a scanner cannot
	// act on.
	if _, _, _, ok := splitToken(token); !ok {
		return "", "", errors.New("minted token does not parse as the token format; refusing to emit it")
	}
	if !matchesScanRegex(token) {
		return "", "", errors.New("minted token is not fully matched by the published secret-scanning regex; refusing to emit it")
	}
	digest := sha256.Sum256([]byte(token))
	return token, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func base62Encode(b []byte) string {
	n := new(big.Int).SetBytes(b)
	base := big.NewInt(62)
	mod := new(big.Int)
	out := make([]byte, 0, base62Width)
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		out = append(out, base62alphabet[mod.Int64()])
	}
	for len(out) < base62Width {
		out = append(out, base62alphabet[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}
