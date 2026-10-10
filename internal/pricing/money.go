// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strings"
)

// Money is an amount in nanodollars: one billionth of one United States dollar.
//
// Money is an integer, and nothing in this package converts through a float.
// Two reasons, both learned the expensive way by other people:
//
//   - Floating point cannot represent $0.30 exactly, so a column of costs
//     summed as float64 disagrees with the same column summed in a different
//     order. A cost report that does not add up is a cost report nobody trusts.
//   - The unit has to be small enough that a cheap request is not zero. One
//     Haiku request of a few hundred tokens costs a fraction of a millionth of
//     a dollar; in microdollars it rounds to nothing, and a million of them
//     still report nothing. Nanodollars keep it.
//
// The range is ±9.2 billion dollars, which is several orders of magnitude more
// than any deployment's lifetime spend.
type Money int64

// nanosPerUSD is the scale of Money: 1 USD == 1e9 nanodollars.
const nanosPerUSD = 1_000_000_000

// tokensPerRateUnit is the denominator of a Rate: rates are quoted per million
// tokens, which is how every provider publishes them.
const tokensPerRateUnit = 1_000_000

// Rate is a price in nanodollars per million tokens.
//
// Per million rather than per token because that is the unit providers publish
// and operators think in - $3.00 per million input tokens - and because a
// per-token integer price would round several current models to the same value.
type Rate int64

// ErrCostOverflow reports token counts and rates whose product cannot be
// represented. It exists so an impossible usage report from a provider surfaces
// as an unpriced request with a loud log line rather than as a silently
// negative cost.
var ErrCostOverflow = errors.New("pricing: cost overflows the representable range")

// ErrNegativeTokens reports a negative token count. Providers do not send them;
// if one ever does, the request is unpriced rather than credited.
var ErrNegativeTokens = errors.New("pricing: negative token count")

// USD renders the amount as an exact decimal string without a currency symbol,
// e.g. "0.02252070". The fraction is always nine digits, because that is the
// full precision Money holds and truncating it in the audit trail would make
// two different costs look identical.
func (m Money) USD() string {
	neg := m < 0
	n := int64(m)
	if neg {
		n = -n
	}

	whole := n / nanosPerUSD
	frac := n % nanosPerUSD

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	fmt.Fprintf(&b, "%d.%09d", whole, frac)
	return b.String()
}

// String renders the amount for a log line.
func (m Money) String() string { return "$" + m.USD() }

// Display renders the amount for a person to read, with a currency symbol and
// as many decimal places as the figure actually needs.
//
// Budgets are quoted in whole cents and coding-agent requests cost millionths
// of a dollar, so one fixed precision cannot serve both: two decimals would
// print every request as $0.00, and nine would print every budget as
// $12.000000000. This is display only - nothing computes with the result, and
// the exact figure is always a USD() call away.
func (m Money) Display() string {
	neg := m < 0
	n := int64(m)
	if neg {
		n = -n
	}

	var out string
	switch {
	case n == 0:
		out = "$0.00"
	case n < nanosPerUSD/100:
		// Below a cent: show enough digits that a small number is a number
		// rather than a rounded-away zero.
		out = "$" + trimTo(Money(n).USD(), 6)
	case n < nanosPerUSD:
		out = "$" + trimTo(Money(n).USD(), 4)
	default:
		// Round the whole amount to cents before splitting it, so $1.999999999
		// reads as $2.00 rather than as $1.99 with the carry dropped.
		cents := (n + nanosPerUSD/200) / (nanosPerUSD / 100)
		out = fmt.Sprintf("$%s.%02d", withThousands(cents/100), cents%100)
	}

	if neg {
		return "-" + out
	}
	return out
}

// trimTo cuts an exact nine-decimal string back to at most n places, keeping at
// least two so the result still reads as money.
func trimTo(s string, n int) string {
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return s
	}
	if len(s)-dot-1 > n {
		s = s[:dot+1+n]
	}
	s = strings.TrimRight(s, "0")
	for len(s)-strings.IndexByte(s, '.')-1 < 2 {
		s += "0"
	}
	return s
}

// withThousands groups a whole number of dollars, because a budget is read at a
// glance and 120000 and 12000 are not distinguishable at a glance.
func withThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}

	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// USDPerMillion renders a rate as an exact decimal string of dollars per
// million tokens, e.g. "3.000000000".
func (r Rate) USDPerMillion() string { return Money(r).USD() }

// String renders a rate for a log line.
func (r Rate) String() string { return "$" + r.USDPerMillion() + "/1M" }

// Cost multiplies a token count by this rate.
//
// The multiplication is done in 128 bits and the division rounds half up, so
// the result is the exact price to the nanodollar rather than something that
// drifts as counts grow. The 128-bit step is not paranoia about realistic
// numbers - it is what lets an absurd usage report return an error instead of
// wrapping around into a negative cost.
func (r Rate) Cost(tokens int64) (Money, error) {
	switch {
	case tokens < 0:
		return 0, fmt.Errorf("%w: %d", ErrNegativeTokens, tokens)
	case tokens == 0 || r == 0:
		return 0, nil
	case r < 0:
		return 0, fmt.Errorf("pricing: negative rate %d", r)
	}

	hi, lo := bits.Mul64(uint64(tokens), uint64(r))
	// bits.Div64 panics when the quotient would not fit in 64 bits. Refusing
	// here turns that into an error the caller can record and log.
	if hi >= tokensPerRateUnit {
		return 0, fmt.Errorf("%w: %d tokens at %s", ErrCostOverflow, tokens, r)
	}

	quo, rem := bits.Div64(hi, lo, tokensPerRateUnit)
	if rem*2 >= tokensPerRateUnit {
		quo++
	}
	if quo > math.MaxInt64 {
		return 0, fmt.Errorf("%w: %d tokens at %s", ErrCostOverflow, tokens, r)
	}
	return Money(quo), nil // #nosec G115 -- bounded by the check above
}

// ParseUSD converts an exact decimal string of dollars into Money.
//
// The string is parsed digit by digit rather than through strconv.ParseFloat,
// so "0.0000008" is exactly 800 nanodollars and not the nearest binary
// approximation of it. Digits below a nanodollar are rounded half up; anything
// that is not a decimal number is an error rather than a zero.
func ParseUSD(s string) (Money, error) {
	n, err := parseScaled(s, 9)
	if err != nil {
		return 0, err
	}
	return Money(n), nil
}

// ParseUSDPerMillion converts an exact decimal string of dollars per million
// tokens into a Rate - the form price tables and operators use ("3.00").
func ParseUSDPerMillion(s string) (Rate, error) {
	n, err := parseScaled(s, 9)
	if err != nil {
		return 0, err
	}
	return Rate(n), nil
}

// ParseUSDPerThousand converts a price quoted per thousand tokens into a Rate.
// The retired half of the AWS Bedrock catalogue quotes prices this way, and
// reading one as though it were per million understates a bill by a thousand.
func ParseUSDPerThousand(s string) (Rate, error) {
	n, err := parseScaled(s, 12)
	if err != nil {
		return 0, err
	}
	return Rate(n), nil
}

// ParseUSDPerToken converts a price quoted per single token into a Rate. AWS
// publishes Bedrock prices per token or per thousand tokens depending on the
// model, so the conversion has to be exact at fifteen decimal places: a
// per-token price of $0.0000008 is $0.80 per million, and losing the tail would
// mis-price every request.
func ParseUSDPerToken(s string) (Rate, error) {
	n, err := parseScaled(s, 15)
	if err != nil {
		return 0, err
	}
	return Rate(n), nil
}

// parseScaled parses a decimal number into an integer scaled by 10^scale,
// rounding half up at the last representable digit.
func parseScaled(s string, scale int) (int64, error) {
	in := strings.TrimSpace(s)
	if in == "" {
		return 0, errors.New("pricing: empty decimal")
	}

	neg := false
	switch in[0] {
	case '-':
		neg, in = true, in[1:]
	case '+':
		in = in[1:]
	}

	whole, frac, _ := strings.Cut(in, ".")
	if whole == "" && frac == "" {
		return 0, fmt.Errorf("pricing: %q is not a decimal number", s)
	}

	value, err := accumulate(whole, 0)
	if err != nil {
		return 0, fmt.Errorf("pricing: %q is not a decimal number: %w", s, err)
	}

	// Scale up by the fraction's worth of digits, consuming the ones that fit.
	for i := range scale {
		var digit int64
		if i < len(frac) {
			d := frac[i]
			if d < '0' || d > '9' {
				return 0, fmt.Errorf("pricing: %q is not a decimal number", s)
			}
			digit = int64(d - '0')
		}
		var ok bool
		if value, ok = mulAdd(value, 10, digit); !ok {
			return 0, fmt.Errorf("pricing: %q is out of range", s)
		}
	}

	// Digits finer than the scale round the last one rather than vanishing.
	if len(frac) > scale {
		for _, d := range frac[scale:] {
			if d < '0' || d > '9' {
				return 0, fmt.Errorf("pricing: %q is not a decimal number", s)
			}
		}
		if frac[scale] >= '5' {
			value++
		}
	}

	if neg {
		value = -value
	}
	return value, nil
}

// accumulate folds a run of decimal digits into an integer, refusing overflow.
func accumulate(digits string, init int64) (int64, error) {
	value := init
	for _, d := range digits {
		if d < '0' || d > '9' {
			return 0, fmt.Errorf("unexpected character %q", d)
		}
		var ok bool
		if value, ok = mulAdd(value, 10, int64(d-'0')); !ok {
			return 0, errors.New("out of range")
		}
	}
	return value, nil
}

// mulAdd computes v*m+add, reporting whether it fits in an int64.
//
// Callers only ever accumulate digits, so v, m and add are non-negative; the
// check makes that an assertion rather than an assumption.
func mulAdd(v, m, add int64) (int64, bool) {
	if v < 0 || m < 0 || add < 0 {
		return 0, false
	}

	hi, lo := bits.Mul64(uint64(v), uint64(m)) // #nosec G115 -- both are non-negative, checked above
	if hi != 0 || lo > math.MaxInt64 {
		return 0, false
	}
	out := int64(lo) + add // #nosec G115 -- bounded by the check above
	if out < 0 {
		return 0, false
	}
	return out, true
}
