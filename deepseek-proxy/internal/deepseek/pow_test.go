package deepseek

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
)

func TestDeepSeekHashV1(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "hello world",
			expected: "d3861d3283d610f532813c30dd15162c6980aed6d34f05aa26caac9baf306f95",
		},
		{
			input:    "test_salt_1700000000_1234",
			expected: "a406aa0b3c998413fbe1d6a7816992f4d7629395c768022af5f52e3871a17c68",
		},
	}

	for _, tt := range tests {
		got, err := DeepSeekHashV1([]byte(tt.input))
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", tt.input, err)
		}
		if got != tt.expected {
			t.Errorf("DeepSeekHashV1(%q) = %s; want %s", tt.input, got, tt.expected)
		}
	}
}

func TestSolvePoW(t *testing.T) {
	challenge := PowChallenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  "a406aa0b3c998413fbe1d6a7816992f4d7629395c768022af5f52e3871a17c68",
		Salt:       "test_salt",
		Difficulty: 2000,
		ExpireAt:   1700000000,
		Signature:  "sig123",
		TargetPath: "/api/v0/chat/completion",
	}

	resBase64, err := SolvePoW(challenge)
	if err != nil {
		t.Fatalf("SolvePoW error: %v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(resBase64)
	if err != nil {
		t.Fatalf("base64 decode error: %v", err)
	}

	var sol PowSolution
	if err := json.Unmarshal(raw, &sol); err != nil {
		t.Fatalf("json unmarshal error: %v", err)
	}

	if sol.Answer != 1234 {
		t.Errorf("sol.Answer = %d; want 1234", sol.Answer)
	}
	if sol.Algorithm != challenge.Algorithm {
		t.Errorf("sol.Algorithm = %s; want %s", sol.Algorithm, challenge.Algorithm)
	}
}

func BenchmarkSolvePoW144k(b *testing.B) {
	// Answer at 100,000
	targetHash, _ := DeepSeekHashV1([]byte("bench_salt_1700000000_100000"))
	challenge := PowChallenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  targetHash,
		Salt:       "bench_salt",
		Difficulty: 144000,
		ExpireAt:   1700000000,
		Signature:  "bench_sig",
		TargetPath: "/api/v0/chat/completion",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := SolvePoW(challenge)
		if err != nil {
			b.Fatal(err)
		}
		_ = res
	}
	_ = fmt.Sprint()
}
