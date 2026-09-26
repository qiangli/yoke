package anthropic

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

// NewStreamReadCloser adapts a streaming response body without buffering it.
// Closing the returned reader also closes the upstream body.
func NewStreamReadCloser(src io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		err := ConvertStream(src, pw)
		_ = src.Close()
		_ = pw.CloseWithError(err)
	}()
	return &streamBody{PipeReader: pr, upstream: src}
}

type streamBody struct {
	*io.PipeReader
	upstream io.Closer
}

func (b *streamBody) Close() error {
	_ = b.upstream.Close()
	return b.PipeReader.Close()
}

// ConvertStream converts OpenAI chat.completion.chunk SSE into Anthropic
// Messages SSE while preserving incremental text and tool arguments.
func ConvertStream(src io.Reader, dst io.Writer) error {
	s := &streamConverter{dst: dst, toolBlocks: map[int]int{}, openBlocks: map[int]bool{}}
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return s.finish()
		}
		var chunk openai.ChatCompletion
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return fmt.Errorf("decode OpenAI stream chunk: %w", err)
		}
		if err := s.chunk(&chunk); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return s.finish()
}

type streamConverter struct {
	dst        io.Writer
	started    bool
	finished   bool
	id         string
	model      string
	usage      openai.Usage
	stop       string
	nextBlock  int
	textBlock  int
	textOpen   bool
	toolBlocks map[int]int
	openBlocks map[int]bool
}

func (s *streamConverter) chunk(chunk *openai.ChatCompletion) error {
	if chunk.ID != "" {
		s.id = chunk.ID
	}
	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.Usage.PromptTokens != 0 || chunk.Usage.CompletionTokens != 0 {
		s.usage = chunk.Usage
	}
	if !s.started {
		s.started = true
		if err := s.event("message_start", MessageStartEvent{Type: "message_start", Message: MessagesResponse{
			ID: s.id, Type: "message", Role: "assistant", Model: s.model, Content: []ContentBlock{},
			Usage: Usage{InputTokens: s.usage.PromptTokens},
		}}); err != nil {
			return err
		}
	}
	for _, choice := range chunk.Choices {
		if choice.Delta != nil {
			if choice.Delta.Content != "" {
				if !s.textOpen {
					s.textBlock, s.textOpen = s.nextBlock, true
					s.nextBlock++
					s.openBlocks[s.textBlock] = true
					if err := s.event("content_block_start", ContentBlockStartEvent{Type: "content_block_start", Index: s.textBlock, ContentBlock: ContentBlock{Type: "text", Text: ""}}); err != nil {
						return err
					}
				}
				if err := s.event("content_block_delta", ContentBlockDeltaEvent{Type: "content_block_delta", Index: s.textBlock, Delta: Delta{Type: "text_delta", Text: choice.Delta.Content}}); err != nil {
					return err
				}
			}
			for _, call := range choice.Delta.ToolCalls {
				block, ok := s.toolBlocks[call.Index]
				if !ok {
					block = s.nextBlock
					s.nextBlock++
					s.toolBlocks[call.Index], s.openBlocks[block] = block, true
					if err := s.event("content_block_start", ContentBlockStartEvent{Type: "content_block_start", Index: block, ContentBlock: ContentBlock{Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: json.RawMessage(`{}`)}}); err != nil {
						return err
					}
				}
				if call.Function.Arguments != "" {
					if err := s.event("content_block_delta", ContentBlockDeltaEvent{Type: "content_block_delta", Index: block, Delta: Delta{Type: "input_json_delta", PartialJSON: call.Function.Arguments}}); err != nil {
						return err
					}
				}
			}
		}
		if choice.FinishReason != nil {
			s.stop = FromOpenAIStop(*choice.FinishReason)
		}
	}
	return nil
}

func (s *streamConverter) finish() error {
	if s.finished {
		return nil
	}
	s.finished = true
	if !s.started {
		s.started = true
		if err := s.event("message_start", MessageStartEvent{Type: "message_start", Message: MessagesResponse{Type: "message", Role: "assistant", Content: []ContentBlock{}}}); err != nil {
			return err
		}
	}
	for i := 0; i < s.nextBlock; i++ {
		if s.openBlocks[i] {
			if err := s.event("content_block_stop", ContentBlockStopEvent{Type: "content_block_stop", Index: i}); err != nil {
				return err
			}
		}
	}
	if s.stop == "" {
		s.stop = "end_turn"
	}
	if err := s.event("message_delta", MessageDeltaEvent{Type: "message_delta", Delta: MessageDelta{StopReason: &s.stop}, Usage: MessageDeltaUsage{OutputTokens: s.usage.CompletionTokens}}); err != nil {
		return err
	}
	return s.event("message_stop", MessageStopEvent{Type: "message_stop"})
}

func (s *streamConverter) event(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.dst, "event: %s\ndata: %s\n\n", name, data)
	return err
}
