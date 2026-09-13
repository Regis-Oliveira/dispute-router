package llm

// Pricing is a provider's rate card, in micro-dollars per million tokens.
//
// Cost is carried in micro-dollars as an int64, for the same reason the ledger
// carries money in minor units: a budget compared with floating point drifts,
// and a ceiling that drifts is not a ceiling. Cents are too coarse here - a run
// can legitimately cost a third of one - so the unit is 1e-6 USD.
type Pricing struct {
	InputMicrosPerMTok  int64
	OutputMicrosPerMTok int64

	// Cached tokens bill at their own rates. Left at zero they are DERIVED from
	// the input rate rather than set equal to it.
	//
	// Falling back to the input rate was the first version and it was safe for a
	// ceiling and useless for a measurement: a cache that worked perfectly would
	// report no saving at all, because every read was billed as a fresh token.
	// The multipliers below are the standard ones - a write costs a quarter more
	// than fresh input, a read a tenth of it - and they are worth checking
	// against current pricing, because every number this system reports about
	// caching is only as right as they are.
	CacheWriteMicrosPerMTok int64
	CacheReadMicrosPerMTok  int64
}

const (
	cacheWriteMultiplier = 1.25
	cacheReadMultiplier  = 0.10
)

// Cost prices one usage in micro-dollars.
func (p Pricing) Cost(u Usage) int64 {
	rate := func(configured int64, multiplier float64) int64 {
		if configured > 0 {
			return configured
		}
		return int64(float64(p.InputMicrosPerMTok) * multiplier)
	}
	const perMTok = 1_000_000
	return int64(u.InputTokens)*p.InputMicrosPerMTok/perMTok +
		int64(u.OutputTokens)*p.OutputMicrosPerMTok/perMTok +
		int64(u.CacheCreationInputTokens)*rate(p.CacheWriteMicrosPerMTok, cacheWriteMultiplier)/perMTok +
		int64(u.CacheReadInputTokens)*rate(p.CacheReadMicrosPerMTok, cacheReadMultiplier)/perMTok
}
