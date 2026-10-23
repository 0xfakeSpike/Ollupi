package llm

import "unicode"

func EstimateTokens(s string) int {
	ascii, nonASCII := 0, 0
	for _, r := range s {
		if r <= unicode.MaxASCII {
			ascii++
		} else {
			nonASCII++
		}
	}
	return (ascii+3)/4 + nonASCII
}
