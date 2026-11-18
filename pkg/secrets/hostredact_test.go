package secrets

import (
	"bytes"
	"strings"
	"testing"
)

func TestHostRedactingWriterIncludesLaterRegistration(t *testing.T) {
	var out bytes.Buffer
	writer := NewHostRedactingWriter(&out)
	first := "synthetic-host-minted-fixture-one"
	second := "synthetic-host-rotated-fixture-two"
	for i, value := range []string{first, second} {
		if err := RegisterHostSecret("HOST_MINTED_TEST", value); err != nil {
			t.Fatal(err)
		}
		split := len(value) / 2
		if _, err := writer.Write([]byte(value[:split])); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(value[split:] + "\n")); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			_, _ = writer.Write([]byte(first))
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), first) || strings.Contains(out.String(), second) {
		t.Fatal("minted or retired credential survived log redaction")
	}
	if strings.Count(out.String(), "[redacted:HOST_MINTED_TEST]") != 3 {
		t.Fatal("registered credentials were not consistently masked")
	}
}

func TestHostCredentialFormsAcrossEveryWriteBoundary(t *testing.T) {
	token := "9f42e8a1b0c7d6358a4f1c962e0b3d75f8a9c6102e4d7b953a6f8c012d35b7e4"
	for _, prefix := range []string{"Authorization: Bearer ", "token=", `{"mcp_bearer_token":"`, "join_secret: "} {
		input := strings.Repeat("ordinary ", 70) + prefix + token + "\nready\n"
		for split := 0; split <= len(input); split++ {
			var out bytes.Buffer
			w := NewHostRedactingWriter(&out)
			_, _ = w.Write([]byte(input[:split]))
			_, _ = w.Write([]byte(input[split:]))
			_ = w.Close()
			if strings.Contains(out.String(), token) {
				t.Fatal("credential form survived a streaming split")
			}
			if !strings.Contains(out.String(), "ready") {
				t.Fatal("noncredential output was lost")
			}
		}
	}
}

func TestHostRedactorLeavesOrdinaryOutput(t *testing.T) {
	input := "token=ordinary-description password=aaaaaaaaaaaaaaaaaaaa status=ready id=9f42e8a1b0c7d6358a4f1c962e0b3d75"
	if got := string(HostRedactor().Redact([]byte(input))); got != input {
		t.Fatal("ordinary output was incorrectly masked")
	}
}

func TestHostRecordWriterMasksWithoutBuffering(t *testing.T) {
	var out bytes.Buffer
	writer := NewHostRecordWriter(&out)
	fixture := "synthetic-record-log-credential"
	if err := RegisterHostSecret("RECORD_TEST", fixture); err != nil {
		t.Fatal(err)
	}
	input := []byte("log " + fixture + "\n")
	n, err := writer.Write(input)
	if err != nil || n != len(input) {
		t.Fatal("record writer violated its input write contract")
	}
	if strings.Contains(out.String(), fixture) || !strings.Contains(out.String(), "[redacted:") {
		t.Fatal("record was not immediately written with credential masked")
	}
}
