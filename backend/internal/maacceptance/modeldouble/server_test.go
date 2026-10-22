package modeldouble

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestModelDoubleFourSessionsOverlapSameCallID(t *testing.T) {
	srv, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	const shared = "call_shared"
	srv.SetScript([]Step{{
		Kind: KindResponsesFunction, Name: "submit_plan", Arguments: `{"items":[]}`,
		CallID: shared, Barrier: "four",
	}})

	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte(`{"input":[{"role":"user","content":"session-` + string(rune('A'+i)) + `"}]}`)
			req, err := http.NewRequest(http.MethodPost, srv.BaseURL()+"/responses", bytes.NewReader(body))
			if err != nil {
				errCh <- err
				return
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errCh <- err
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte(shared)) {
				t.Errorf("session %d status=%d body=%s", i, resp.StatusCode, raw)
			}
		}(i)
	}
	if !srv.Barrier("four").WaitArrived(4, 2*time.Second) {
		t.Fatalf("expected 4 overlapping sessions, arrived=%d peak=%d", srv.Barrier("four").Arrived(), srv.Peak())
	}
	if srv.Peak() < 4 {
		t.Fatalf("peak in flight = %d, want >= 4", srv.Peak())
	}
	srv.Barrier("four").Release()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	events := srv.Events()
	if len(events) != 4 {
		t.Fatalf("events=%d", len(events))
	}
	for _, ev := range events {
		if len(ev.CallIDs) != 1 || ev.CallIDs[0] != shared {
			t.Fatalf("call ids = %v", ev.CallIDs)
		}
	}
	if srv.InFlight() != 0 {
		t.Fatalf("in flight after release = %d", srv.InFlight())
	}
}

func TestModelDoubleScriptedFaultsAndCancel(t *testing.T) {
	srv, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetScript([]Step{
		{Match: "incomplete-args", Kind: KindIncompleteArgs, Name: "http_request", Arguments: `{"url":`, CallID: "call_bad"},
		{Match: "incomplete-status", Kind: KindIncompleteStatus, Name: "http_request", Arguments: `{}`, CallID: "call_status"},
		{Match: "rate-limit", Kind: KindStatus, StatusCode: 429},
		{Match: "upstream", Kind: KindStatus, StatusCode: 503},
		{Match: "refuse", Kind: KindRefusal},
		{Match: "cut-off", Kind: KindTruncated, Name: "http_request", Arguments: `{"url":"http://127.0.0.1/x"}`},
		{Match: "hang", Kind: KindTimeout, Barrier: "hang"},
	})

	post := func(path, marker string) (*http.Response, []byte) {
		t.Helper()
		resp, err := http.Post(srv.BaseURL()+path, "application/json", bytes.NewReader([]byte(`{"input":"`+marker+`"}`)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, raw
	}
	if resp, raw := post("/responses", "incomplete-args"); resp.StatusCode != 200 || !bytes.Contains(raw, []byte("call_bad")) || !bytes.Contains(raw, []byte("in_progress")) || !bytes.Contains(raw, []byte("url")) {
		t.Fatalf("incomplete args: %d %s", resp.StatusCode, raw)
	}
	if resp, raw := post("/chat/completions", "incomplete-status"); resp.StatusCode != 200 || !bytes.Contains(raw, []byte("http_request")) {
		t.Fatalf("chat incomplete: %d %s", resp.StatusCode, raw)
	}
	if resp, _ := post("/responses", "rate-limit"); resp.StatusCode != 429 {
		t.Fatalf("429 got %d", resp.StatusCode)
	}
	if resp, _ := post("/responses", "upstream"); resp.StatusCode != 503 {
		t.Fatalf("503 got %d", resp.StatusCode)
	}
	if resp, raw := post("/responses", "refuse"); resp.StatusCode != 200 || !bytes.Contains(raw, []byte("refusal")) {
		t.Fatalf("refusal: %d %s", resp.StatusCode, raw)
	}
	if resp, raw := post("/responses", "cut-off"); resp.StatusCode != 200 || !bytes.Contains(raw, []byte("incomplete")) {
		t.Fatalf("truncated: %d %s", resp.StatusCode, raw)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.BaseURL()+"/responses", bytes.NewReader([]byte(`{"input":"hang"}`)))
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected cancel error")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && srv.InFlight() != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	foundCancel := false
	for _, ev := range srv.Events() {
		if ev.Cancelled {
			foundCancel = true
		}
	}
	if !foundCancel {
		t.Fatal("cancel was not recorded")
	}
}

func TestModelDoubleDoesNotExposeCredentialsInTrace(t *testing.T) {
	srv, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetScript([]Step{{Kind: KindText, Text: "ok"}})
	req, _ := http.NewRequest(http.MethodPost, srv.BaseURL()+"/chat/completions", bytes.NewReader([]byte(`{"messages":[{"role":"user","content":"secret-prompt"}]}`)))
	req.Header.Set("Authorization", "Bearer local-test-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	raw, _ := json.Marshal(srv.Events())
	if bytes.Contains(raw, []byte("local-test-secret")) || bytes.Contains(raw, []byte("secret-prompt")) {
		t.Fatalf("trace leaked request material: %s", raw)
	}
}
