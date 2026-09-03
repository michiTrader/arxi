package designer

import "testing"

// typing folds a string into a field the way the adapter folds keystrokes, so a
// test can say what was typed instead of building the struct.
func typing(s string) field {
	f := field{}
	for _, r := range s {
		f = f.insert(r)
	}
	return f
}

func TestTheCursorEditsInTheMiddleAndNotOnlyAtTheEnd(t *testing.T) {
	f := typing("bakend")
	for i := 0; i < 4; i++ {
		f = f.left()
	}
	f = f.insert('c')
	if got := f.String(); got != "backend" {
		t.Fatalf("typed \"bakend\", moved left four times and inserted 'c'\n"+
			"  want: %q\n  got:  %q\n"+
			"  a field that can only append is a field where a typo means retyping the name.",
			"backend", got)
	}
	if f.pos != 3 {
		t.Errorf("the cursor is at %d after inserting; it belongs after the rune just typed (3)", f.pos)
	}
}

func TestBackspaceAndDeleteAreDifferentKeys(t *testing.T) {
	f := typing("abc").left() // between b and c
	if got := f.backspace().String(); got != "ac" {
		t.Errorf("backspace with the cursor before 'c' should remove 'b'; got %q", got)
	}
	if got := f.del().String(); got != "ab" {
		t.Errorf("delete with the cursor before 'c' should remove 'c'; got %q", got)
	}
}

func TestEditingCountsRunesAndNotBytes(t *testing.T) {
	// "revisión" is 8 runes and 9 bytes. A byte-indexed field leaves half of the
	// ó behind, and the frame then shows a replacement glyph nobody typed.
	f := typing("revisión")
	if got := f.backspace().String(); got != "revisió" {
		t.Fatalf("one backspace on %q\n  want: %q\n  got:  %q\n"+
			"  editing by byte splits a multi-byte rune and puts an invalid one on screen.",
			"revisión", "revisió", got)
	}
	f = f.home().right().right().right()
	if got := f.insert('X').String(); got != "revXisión" {
		t.Errorf("home then three rights then insert: want %q, got %q", "revXisión", got)
	}
}

func TestAControlRuneNeverEntersTheText(t *testing.T) {
	for _, r := range []rune{0, '\t', '\r', '\n', 0x1b, 0x7f} {
		if got := typing("a").insert(r).String(); got != "a" {
			t.Errorf("insert(%#x) stored the rune: got %q\n"+
				"  raw mode delivers bytes nobody typed, and one of them inside a name\n"+
				"  travels through the model into a YAML file no editor will show honestly.",
				r, got)
		}
	}
}

func TestTheCursorStopsAtBothEndsInsteadOfWrappingOrPanicking(t *testing.T) {
	f := typing("ab").home()
	if f.left().pos != 0 {
		t.Errorf("left at the start moved the cursor to %d", f.left().pos)
	}
	if got := f.backspace().String(); got != "ab" {
		t.Errorf("backspace at the start changed the text to %q", got)
	}
	f = f.end()
	if f.right().pos != 2 {
		t.Errorf("right at the end moved the cursor to %d", f.right().pos)
	}
	if got := f.del().String(); got != "ab" {
		t.Errorf("delete at the end changed the text to %q", got)
	}
	if !(field{}).empty() || typing("a").empty() {
		t.Error("empty() disagrees with whether there is any text")
	}
}
