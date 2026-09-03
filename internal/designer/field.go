package designer

// field is one line of text with a cursor in it.
//
// It is its own type with its own tests because the alternative -- a string the
// model appends to -- is the difference between a text box and a typing
// exercise. Anybody who has typed a name, seen a typo in the middle of it and
// pressed the left arrow finds out in one second whether this type exists.
//
// The unit is a rune and never a byte. pos counts runes and every operation is
// expressed in them, so a name with an accent in it loses one character per
// backspace instead of leaving half a rune behind and a replacement glyph on
// screen.
//
// Every method returns a new field rather than mutating the receiver, for the
// same reason Update returns a new Model: a keystroke sequence in a test is then
// a fold, and an assertion about the third key cannot be disturbed by the fifth.
type field struct {
	text []rune
	pos  int // 0..len(text); len(text) is "after the last rune"
}

func newField(s string) field {
	r := []rune(s)
	return field{text: r, pos: len(r)}
}

// String is the text alone. The cursor is drawn by Render, which is told where
// it is; a field that rendered its own caret could not be reused in a frame that
// is not the focused one.
func (f field) String() string { return string(f.text) }

func (f field) empty() bool { return len(f.text) == 0 }

// insert puts r at the cursor and leaves the cursor after it.
//
// Control runes are dropped instead of stored. A terminal in raw mode delivers
// bytes nobody typed -- a bracketed-paste marker, the tail of an escape sequence
// this decoder does not know -- and one of them inside the text would travel
// through the model into a YAML file, where the damage is a blueprint whose name
// contains a byte no editor will show.
func (f field) insert(r rune) field {
	if r < 0x20 || r == 0x7f {
		return f
	}
	out := make([]rune, 0, len(f.text)+1)
	out = append(out, f.text[:f.pos]...)
	out = append(out, r)
	out = append(out, f.text[f.pos:]...)
	return field{text: out, pos: f.pos + 1}
}

// backspace removes the rune before the cursor.
func (f field) backspace() field {
	if f.pos == 0 {
		return f
	}
	out := append([]rune{}, f.text[:f.pos-1]...)
	out = append(out, f.text[f.pos:]...)
	return field{text: out, pos: f.pos - 1}
}

// del removes the rune at the cursor, which is what the Delete key means and
// what Backspace does not do.
func (f field) del() field {
	if f.pos >= len(f.text) {
		return f
	}
	out := append([]rune{}, f.text[:f.pos]...)
	out = append(out, f.text[f.pos+1:]...)
	return field{text: out, pos: f.pos}
}

func (f field) left() field {
	if f.pos > 0 {
		f.pos--
	}
	return f
}

func (f field) right() field {
	if f.pos < len(f.text) {
		f.pos++
	}
	return f
}

func (f field) home() field { f.pos = 0; return f }
func (f field) end() field  { f.pos = len(f.text); return f }
