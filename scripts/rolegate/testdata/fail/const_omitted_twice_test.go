// Test fixture: two consecutive omitted const specs keep repeating the full list.
package fail

const (
	firstWord, secondWord = "CREATE ", "ROLE fixture_omitted_twice"
	omittedFirst, omittedSecond
	copiedFirst, copiedSecond
)

var omittedTwiceStatement = copiedFirst + copiedSecond
