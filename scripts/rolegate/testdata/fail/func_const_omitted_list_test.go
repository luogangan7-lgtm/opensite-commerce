// Test fixture: the same omitted-list semantics apply inside a function body.
package fail

func localOmittedList(ctx, db any) {
	const (
		prefix, tail = "CREATE ", "ROLE fixture_func_omitted"
		begin, end   // omitted: repeats both values by position
	)
	db.Exec(ctx, begin+end)
}
