package core

import "strconv"

// CompactTokens renders a token count as 999, 12.3k, 217.6k or 1.4M, dropping a
// zero decimal (256k, not 256.0k). It rounds half up on integers, so 999_950
// reads 1M rather than 1000k, and negative counts, which no provider should
// report, read 0. Every surface that shows a token count uses it, so the same
// figure never renders two ways.
func CompactTokens(n int) string {
	n = max(n, 0)
	if n < 1000 {
		return strconv.Itoa(n)
	}
	tenths, unit := RoundDiv(n, 100), "k"
	if tenths >= 10_000 {
		tenths, unit = RoundDiv(n, 100_000), "M"
	}
	if tenths%10 == 0 {
		return strconv.Itoa(tenths/10) + unit
	}
	return strconv.Itoa(tenths/10) + "." + strconv.Itoa(tenths%10) + unit
}

// RoundDiv divides a non-negative n by d, rounding half up, without the
// overflow that adding d/2 first could cause near the top of the range.
func RoundDiv(n, d int) int {
	q := n / d
	if n%d >= (d+1)/2 {
		q++
	}
	return q
}
