// Test fixture: `+` concatenation must be folded before matching.
package fail

func statement(name string) string {
	return "CREATE " + "ROLE " + name
}
