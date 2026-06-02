package service

import (
	"errors"
	"strings"
	"testing"
)

// relayImageBridgeSSE is the testable core of ProxyImageGenViaHTTP: it reads an upstream
// SSE stream, relays each data event to the client via the write callback, and reports
// (a) a forward result for billing and (b) whether anything was written downstream — the
// latter gates the caller's fall-through-to-WS decision.

func TestRelayImageBridgeSSE_RelaysDataAndExtractsUsage(t *testing.T) {
	stream := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_abc"}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_abc","usage":{"input_tokens":120,"output_tokens":30,"output_tokens_details":{"image_tokens":25}},"output":[{"type":"image_generation_call","id":"ig_1","result":"AAAA","size":"1024x1024"}]}}`,
		"",
	}, "\n")

	var written [][]byte
	write := func(data []byte) error {
		written = append(written, append([]byte(nil), data...))
		return nil
	}

	svc := &OpenAIGatewayService{}
	result, wrote, err := svc.relayImageBridgeSSE(strings.NewReader(stream), write, "gpt-5.4", "gpt-5.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wrote {
		t.Fatalf("expected wroteDownstream=true")
	}
	if len(written) != 2 {
		t.Fatalf("expected 2 relayed data events, got %d: %q", len(written), written)
	}
	// event: lines and blank lines must not be relayed.
	for _, w := range written {
		if strings.HasPrefix(string(w), "event:") || strings.TrimSpace(string(w)) == "" {
			t.Fatalf("relayed a non-data line: %q", w)
		}
	}
	if result == nil {
		t.Fatal("expected a forward result")
	}
	if result.Usage.InputTokens != 120 || result.Usage.OutputTokens != 30 {
		t.Fatalf("usage not extracted: %+v", result.Usage)
	}
	if result.Usage.ImageOutputTokens != 25 {
		t.Fatalf("image output tokens not extracted: %+v", result.Usage)
	}
	if result.ImageCount != 1 {
		t.Fatalf("expected ImageCount=1, got %d", result.ImageCount)
	}
	if result.ResponseID != "resp_abc" {
		t.Fatalf("expected ResponseID=resp_abc, got %q", result.ResponseID)
	}
	if !result.OpenAIWSMode {
		t.Fatal("expected OpenAIWSMode=true so usage records as a WS turn")
	}
}

func TestRelayImageBridgeSSE_StopsAtDoneWithoutRelayingIt(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		"data: [DONE]",
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":1}}}`,
	}, "\n")

	var written []string
	write := func(data []byte) error {
		written = append(written, string(data))
		return nil
	}

	svc := &OpenAIGatewayService{}
	_, wrote, err := svc.relayImageBridgeSSE(strings.NewReader(stream), write, "m", "m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wrote {
		t.Fatal("expected wroteDownstream=true")
	}
	if len(written) != 1 || written[0] != `{"type":"response.output_text.delta","delta":"hi"}` {
		t.Fatalf("should relay only the pre-DONE data line, got %q", written)
	}
}

func TestRelayImageBridgeSSE_ClientDisconnectMarksWrittenNoError(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"a"}`,
		`data: {"type":"response.output_text.delta","delta":"b"}`,
	}, "\n")

	write := func(data []byte) error {
		return errors.New("client gone")
	}

	svc := &OpenAIGatewayService{}
	_, wrote, err := svc.relayImageBridgeSSE(strings.NewReader(stream), write, "m", "m")
	if err != nil {
		t.Fatalf("client disconnect should not surface as error: %v", err)
	}
	// A write was attempted: the client already owns this turn, so the caller must NOT
	// fall through to the normal WS path.
	if !wrote {
		t.Fatal("expected wroteDownstream=true after a write attempt")
	}
}

func TestRelayImageBridgeSSE_NoDataBeforeErrorAllowsFallthrough(t *testing.T) {
	// Upstream sends only comments/events then the stream errors before any data event.
	// Nothing was written downstream, so the caller may safely fall through to WS.
	svc := &OpenAIGatewayService{}
	r := &errReader{data: "event: response.created\n", err: errors.New("boom")}
	_, wrote, err := svc.relayImageBridgeSSE(r, func([]byte) error { return nil }, "m", "m")
	if wrote {
		t.Fatal("expected wroteDownstream=false when nothing relayed")
	}
	if err == nil {
		t.Fatal("expected the upstream read error to surface for fall-through decision")
	}
}

func TestRelayImageBridgeSSE_EOFAfterCompletedIsSuccess(t *testing.T) {
	// chatgpt.com closes abruptly after response.completed without a [DONE] sentinel.
	svc := &OpenAIGatewayService{}
	r := &errReader{
		data: `data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n",
		err:  errors.New("unexpected EOF"),
	}
	result, wrote, err := svc.relayImageBridgeSSE(r, func([]byte) error { return nil }, "m", "m")
	if err != nil {
		t.Fatalf("EOF after response.completed must be treated as success, got %v", err)
	}
	if !wrote {
		t.Fatal("expected wroteDownstream=true")
	}
	if result == nil || result.Usage.InputTokens != 1 {
		t.Fatalf("expected usage from completed event, got %+v", result)
	}
}

func TestApplyImageBridgeBillingModel_AlignsWithWSPath(t *testing.T) {
	svc := &OpenAIGatewayService{}
	// The bridge HTTP body carries the injected image_generation tool (no explicit
	// model/size), exactly like the WS path's injected payload, so billing must resolve
	// to the same image billing model rather than the coding request model.
	body := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","output_format":"png"}],"input":[]}`)

	result := &OpenAIForwardResult{Model: "gpt-5.4", ImageCount: 1}
	svc.applyImageBridgeBillingModel(result, body, "gpt-5.4")
	if result.BillingModel != "gpt-image-2" {
		t.Fatalf("expected BillingModel=gpt-image-2 (aligned with WS path), got %q", result.BillingModel)
	}
}

func TestApplyImageBridgeBillingModel_NoImageNoChange(t *testing.T) {
	svc := &OpenAIGatewayService{}
	result := &OpenAIForwardResult{Model: "gpt-5.4"} // ImageCount == 0
	svc.applyImageBridgeBillingModel(result, []byte(`{"model":"gpt-5.4"}`), "gpt-5.4")
	if result.BillingModel != "" {
		t.Fatalf("expected BillingModel untouched for non-image turn, got %q", result.BillingModel)
	}
}

// errReader yields data once, then returns err on the next read.
type errReader struct {
	data string
	err  error
	done bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	n := copy(p, r.data)
	return n, nil
}
