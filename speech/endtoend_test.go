package speech

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"

	"livetranscribebot/captions"
)

// The captions package's posters are speech Outputs (checked here rather than
// there, so captions doesn't need to import speech).
var (
	_ Output = (*captions.ThreadPoster)(nil)
	_ Output = captions.TerminalOutput{}
)

// threadMessages stands in for Discord's side of a captions thread: it keeps
// each message's current text.
type threadMessages struct {
	rest.Rest
	mu       sync.Mutex
	messages map[snowflake.ID]string
	order    []snowflake.ID
}

func (f *threadMessages) CreateMessage(channelID snowflake.ID, m discord.MessageCreate, _ ...rest.RequestOpt) (*discord.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := snowflake.ID(100 + len(f.order))
	f.messages[id] = m.Content
	f.order = append(f.order, id)
	return &discord.Message{ID: id, ChannelID: channelID}, nil
}

func (f *threadMessages) UpdateMessage(channelID, messageID snowflake.ID, m discord.MessageUpdate, _ ...rest.RequestOpt) (*discord.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages[messageID] = *m.Content
	return &discord.Message{ID: messageID, ChannelID: channelID}, nil
}

func (f *threadMessages) shown() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var texts []string
	for _, id := range f.order {
		texts = append(texts, f.messages[id])
	}
	return texts
}

// The whole path with real speech: the model's sample recording, as Discord
// packets, through the transcriber and the captions poster into the thread.
func TestSpeechEndsUpAsOneFinishedMessageInTheThread(t *testing.T) {
	recogniser := testRecogniser(t) // skips if the model isn't downloaded
	thread := &threadMessages{messages: map[snowflake.ID]string{}}
	poster := captions.NewThreadPoster(thread, 7001, func(snowflake.ID) string { return "Sam" }, nil)
	tr := NewTranscriber(recogniser, poster, nobodyOptedOut)

	for _, data := range speechPackets(t, sampleWAV("0.wav")) {
		if err := tr.ReceiveOpusFrame(1, &voice.Packet{SSRC: 7, Opus: data}); err != nil {
			t.Fatal(err)
		}
	}
	tr.Close() // the call ends mid-pause: the last sentence is finished and sent
	select {
	case <-poster.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the poster never finished")
	}

	shown := thread.shown()
	if len(shown) != 1 {
		t.Fatalf("want the sentence as one message, got %q", shown)
	}
	t.Logf("thread shows: %s", shown[0])
	if !strings.HasPrefix(shown[0], "**Sam:** ") || strings.Contains(shown[0], "speaking…") {
		t.Errorf("want a finished caption under Sam's name, got %q", shown[0])
	}
	containsAll(t, shown[0], "AFTER EARLY NIGHTFALL", "YELLOW LAMPS", "BROTHELS")
}
