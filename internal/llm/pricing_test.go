package llm

import "testing"

// A cache read is billed at a fraction of an input token, and getting that
// wrong would report a saving that did not happen - or, as the first version
// did, hide one that did.
func TestCachedTokensAreBilledAtTheirOwnRate(t *testing.T) {
	p := Pricing{
		InputMicrosPerMTok:      3_000_000,
		OutputMicrosPerMTok:     15_000_000,
		CacheWriteMicrosPerMTok: 3_750_000,
		CacheReadMicrosPerMTok:  300_000,
	}

	// A million tokens read from cache costs a tenth of a million fresh ones.
	if got := p.Cost(Usage{CacheReadInputTokens: 1_000_000}); got != 300_000 {
		t.Errorf("cache read cost %d, want 300000", got)
	}
	// Writing costs more than reading fresh, which is what makes the first call
	// of a run dearer than the ones after it.
	if got := p.Cost(Usage{CacheCreationInputTokens: 1_000_000}); got != 3_750_000 {
		t.Errorf("cache write cost %d, want 3750000", got)
	}
}

// Unset cache rates are derived from the input rate, not set equal to it.
//
// Equal was the first version. It was safe for a ceiling and useless for a
// measurement: a cache working perfectly would have reported no saving at all.
func TestUnsetCacheRatesAreDerived(t *testing.T) {
	p := Pricing{InputMicrosPerMTok: 3_000_000, OutputMicrosPerMTok: 15_000_000}

	if got := p.Cost(Usage{CacheReadInputTokens: 1_000_000}); got != 300_000 {
		t.Errorf("derived cache read cost %d, want a tenth of input (300000)", got)
	}
	if got := p.Cost(Usage{CacheCreationInputTokens: 1_000_000}); got != 3_750_000 {
		t.Errorf("derived cache write cost %d, want a quarter more than input (3750000)", got)
	}
	// A million fresh tokens still cost what they cost.
	if got := p.Cost(Usage{InputTokens: 1_000_000}); got != 3_000_000 {
		t.Errorf("input cost %d", got)
	}
}

// A configured cache rate wins over the derived one, so a provider whose
// pricing differs can be described without editing code.
func TestAConfiguredCacheRateWins(t *testing.T) {
	p := Pricing{InputMicrosPerMTok: 3_000_000, OutputMicrosPerMTok: 15_000_000}
	p.CacheReadMicrosPerMTok = 150_000
	if got := p.Cost(Usage{CacheReadInputTokens: 1_000_000}); got != 150_000 {
		t.Errorf("configured cache read rate cost %d, want 150000", got)
	}
}
