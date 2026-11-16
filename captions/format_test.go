package captions

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMessage(t *testing.T) {
	tests := []struct {
		why           string
		name, text    string
		final, notice bool
		want          string
	}{
		{"still talking", "Sam", "so I rang", false, false, "**Sam:** so I rang\n-# speaking…"},
		{"finished", "Sam", "So I rang him.", true, false, "**Sam:** So I rang him."},
		{"swearing as said", "Sam", "Fuck that.", true, false, "**Sam:** Fuck that."},
		{"formatting characters in a name show as typed", "xX_Sam_Xx", "hi", true, false, `**xX\_Sam\_Xx:** hi`},
		{"and in the text", "Sam", `a*b ~c~ |d| \e`, true, false, `**Sam:** a\*b \~c\~ \|d\| \\e`},
		{"opt-out notice", "Jo_", "", true, true, `*[Jo\_ isn't being captioned]*`},
	}
	for _, test := range tests {
		if got := Message(test.name, test.text, test.final, test.notice); got != test.want {
			t.Errorf("%s: got %q, want %q", test.why, got, test.want)
		}
	}

	// Discord refuses messages over 2000 characters.
	long := Message("Sam", strings.Repeat("word ", 1000), false, false)
	if n := utf8.RuneCountInString(long); n > 2000 {
		t.Errorf("a long caption is %d characters, over Discord's 2000", n)
	}
	if !strings.HasSuffix(long, "…"+SpeakingLine) {
		t.Errorf("a long caption should be cut with … and keep its speaking line, ends %q", long[len(long)-40:])
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		why           string
		content       string
		speaker, text string
		ok            bool
	}{
		{"a finished caption", "**Sam:** So I rang him.", "Sam", "So I rang him.", true},
		{"escaped name back to how it's typed", `**xX\_Sam\_Xx:** hi \*there\*`, "xX_Sam_Xx", "hi *there*", true},
		{"a fixed caption: the fixed-by line isn't part of the text", "**Priya:** that's great [laughs]\n-# ✏️ fixed by Jo", "Priya", "that's great [laughs]", true},
		{"a typed message", "**Jo (typed):** can you hear me", "Jo (typed)", "can you hear me", true},
		{"several lines", "**Sam:** one\ntwo", "Sam", "one\ntwo", true},
		{"the thread's opening post", "📝 **Live captions: Weekly**\nI'm captioning…", "", "", false},
		{"the opt-out notice", "*[Jo isn't being captioned]*", "", "", false},
		{"no name", "**:** hello", "", "", false},
		{"bold but not a caption", "**Important** read this", "", "", false},
		{"plain text", "hello", "", "", false},
	}
	for _, test := range tests {
		speaker, text, ok := Parse(test.content)
		if speaker != test.speaker || text != test.text || ok != test.ok {
			t.Errorf("%s: got (%q, %q, %v), want (%q, %q, %v)", test.why, speaker, text, ok, test.speaker, test.text, test.ok)
		}
	}
}

func TestFixed(t *testing.T) {
	got, err := Fixed("  Priya ", " that's great ", []string{"[laughs]", "[sarcastic]"}, "Jo")
	if want := "**Priya:** that's great [laughs] [sarcastic]\n-# ✏️ fixed by Jo"; err != nil || got != want {
		t.Errorf("got (%q, %v), want %q", got, err, want)
	}

	// Names and text show as typed, and it reads back to the same thing.
	got, _ = Fixed("xX_Sam_Xx", "a *b*", nil, "Jo_")
	if want := `**xX\_Sam\_Xx:** a \*b\*` + "\n" + `-# ✏️ fixed by Jo\_`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if speaker, text, ok := Parse(got); !ok || speaker != "xX_Sam_Xx" || text != "a *b*" {
		t.Errorf("a fixed caption should read back as typed, got (%q, %q, %v)", speaker, text, ok)
	}

	if _, err := Fixed("  ", "hi", nil, "Jo"); !errors.Is(err, ErrBlank) {
		t.Errorf("a blank speaker should be refused, got %v", err)
	}
	if _, err := Fixed("Sam", " \n ", nil, "Jo"); !errors.Is(err, ErrBlank) {
		t.Errorf("a blank caption should be refused, got %v", err)
	}
	if _, err := Fixed("Sam", strings.Repeat("x", MaxMessageLength), nil, "Jo"); !errors.Is(err, ErrTooLong) {
		t.Errorf("a caption over Discord's limit should be refused, got %v", err)
	}
}

func TestUnescapeMarkdownUndoesEscapeMarkdown(t *testing.T) {
	for _, text := range []string{"plain", `a\b`, "x_y*z~w|v`u", `trailing\`, `\\`, "[laughs] 50% *cheers*"} {
		if got := UnescapeMarkdown(EscapeMarkdown(text)); got != text {
			t.Errorf("round trip of %q gave %q", text, got)
		}
	}
	// A backslash that isn't escaping anything is left alone.
	if got := UnescapeMarkdown(`a\b \`); got != `a\b \` {
		t.Errorf("got %q", got)
	}
}

func TestCutToLength(t *testing.T) {
	if got := CutToLength("short", 10); got != "short" {
		t.Errorf("text that fits shouldn't change, got %q", got)
	}
	if got := CutToLength("📝 abcdef", 5); got != "📝 ab…" {
		t.Errorf("want 5 characters ending in …, counting the emoji as one, got %q", got)
	}
}

// Split gives the caption's parts exactly as the message holds them.
func TestSplit(t *testing.T) {
	speaker, text, ok := Split(`**xX\_Sam\_Xx:** a \*b\*` + "\n-# ✏️ fixed by Jo")
	if !ok || speaker != `xX\_Sam\_Xx` || text != `a \*b\*` {
		t.Errorf("got (%q, %q, %v)", speaker, text, ok)
	}
	if _, _, ok := Split("**:** no name"); ok {
		t.Error("a caption needs a name")
	}
}
