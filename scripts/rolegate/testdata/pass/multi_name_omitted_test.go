// Test fixture: a legal non-role multi-name omitted const list stays clean.
package pass

const (
	firstPart, secondPart = "alpha", "beta"
	thirdPart, fourthPart // omitted: repeats both legal values
)

var plainJoined = thirdPart + fourthPart
