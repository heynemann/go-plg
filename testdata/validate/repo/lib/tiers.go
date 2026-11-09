// Package lib holds what the repository's Go plugins share.
package lib

// Gold is the tier of the users the gold cohort holds.
const Gold = "gold"

// IsGold reports whether tier is the gold tier.
func IsGold(tier string) bool {
	return tier == Gold
}
