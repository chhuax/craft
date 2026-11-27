package protocol

import (
	"encoding/json"
	"maps"
)

type ThreadRef struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
	ID     string `json:"id"`
}

type ActorRef struct {
	Source string  `json:"source"`
	ID     string  `json:"id"`
	Name   *string `json:"name,omitempty"`
}

// ChannelRef 保留扩展提供的完整回复地址；thread 的形状以及附加字段由扩展解释。
type ChannelRef struct {
	Source string                     `json:"source"`
	Thread json.RawMessage            `json:"thread"`
	Extra  map[string]json.RawMessage `json:"-"`
}

func (c *ChannelRef) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var source string
	if err := json.Unmarshal(fields["source"], &source); err != nil {
		return err
	}
	thread := fields["thread"]
	delete(fields, "source")
	delete(fields, "thread")
	*c = ChannelRef{Source: source, Thread: thread, Extra: fields}
	return nil
}

func (c ChannelRef) MarshalJSON() ([]byte, error) {
	fields := maps.Clone(c.Extra)
	if fields == nil {
		fields = make(map[string]json.RawMessage, 2)
	}
	source, err := json.Marshal(c.Source)
	if err != nil {
		return nil, err
	}
	fields["source"] = source
	fields["thread"] = c.Thread
	return json.Marshal(fields)
}

// Trigger 是跨进程传递的事件快照。addressed 允许显式 null，meta 中的数值应保留原精度。
type Trigger struct {
	ID        string                     `json:"id"`
	Source    string                     `json:"source"`
	Event     string                     `json:"event"`
	Threads   []ThreadRef                `json:"threads"`
	Actor     ActorRef                   `json:"actor"`
	Human     bool                       `json:"human"`
	Addressed *string                    `json:"addressed"`
	At        string                     `json:"at"`
	Text      string                     `json:"text"`
	Channel   *ChannelRef                `json:"channel,omitempty"`
	URL       *string                    `json:"url,omitempty"`
	Meta      map[string]json.RawMessage `json:"meta"`
}
