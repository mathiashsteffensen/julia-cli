package core

import (
	"reflect"
	"testing"
)

func TestJoinPromptsRenumbersEachMessagesImages(t *testing.T) {
	a, b, c := Image{Data: []byte("a")}, Image{Data: []byte("b")}, Image{Data: []byte("c")}
	joined := JoinPrompts([]Prompt{
		{Text: "[Image #1] and [Image #2]", Images: []Image{a, b}},
		{Text: "no images, [Image #1] is literal"},
		{Text: "see [Image #1], not [Image #4]", Images: []Image{c}},
	})
	want := "[Image #1] and [Image #2]\n\nno images, [Image #1] is literal\n\nsee [Image #3], not [Image #4]"
	if joined.Text != want {
		t.Fatalf("text %q", joined.Text)
	}
	if !reflect.DeepEqual(joined.Images, []Image{a, b, c}) {
		t.Fatalf("images %+v", joined.Images)
	}
}

func TestJoinPromptsDropsSteering(t *testing.T) {
	if p := JoinPrompts([]Prompt{{Text: "x", Steering: NewInbox()}}); p.Steering != nil || p.Text != "x" {
		t.Fatalf("joined %+v", p)
	}
}

func TestInboxReturnsUndeliveredMessagesAheadOfNewer(t *testing.T) {
	b := NewInbox()
	b.Send(Prompt{Text: "one"})
	b.Send(Prompt{Text: "two"})
	select {
	case <-b.Ready():
	default:
		t.Fatal("not ready after Send")
	}
	taken := b.Take()
	b.Send(Prompt{Text: "three"})
	b.Return(taken)
	var texts []string
	for _, p := range b.Take() {
		texts = append(texts, p.Text)
	}
	if !reflect.DeepEqual(texts, []string{"one", "two", "three"}) {
		t.Fatalf("order %q", texts)
	}
	if len(b.Take()) != 0 {
		t.Fatal("Take left messages behind")
	}
}
