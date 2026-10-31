package deepseek

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const sha3Rate = 136

var keccakRC = []uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808A, 0x8000000080008000,
	0x000000000000808B, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008A, 0x0000000000000088, 0x0000000080008009, 0x000000008000000A,
	0x000000008000808B, 0x800000000000008B, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800A, 0x800000008000000A,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

var rotTable = [5][5]int{
	{0, 36, 3, 41, 18},
	{1, 44, 10, 45, 2},
	{62, 6, 43, 15, 61},
	{28, 55, 25, 21, 56},
	{27, 20, 39, 8, 14},
}

type rhoPiEntry struct {
	src int
	dst int
	rot int
}

var rhoPiTable [25]rhoPiEntry

func init() {
	idx := 0
	for x := 0; x < 5; x++ {
		for y := 0; y < 5; y++ {
			rhoPiTable[idx] = rhoPiEntry{
				src: x + 5*y,
				dst: y + 5*((2*x+3*y)%5),
				rot: rotTable[x][y],
			}
			idx++
		}
	}
}

func keccakF(A *[25]uint64, roundConstants []uint64) {
	var C [5]uint64
	var D [5]uint64
	var B [25]uint64

	for _, rc := range roundConstants {
		// Theta
		for x := 0; x < 5; x++ {
			C[x] = A[x] ^ A[x+5] ^ A[x+10] ^ A[x+15] ^ A[x+20]
		}
		for x := 0; x < 5; x++ {
			D[x] = C[(x+4)%5] ^ bits.RotateLeft64(C[(x+1)%5], 1)
		}
		for i := 0; i < 25; i++ {
			A[i] ^= D[i%5]
		}

		// Rho & Pi
		for _, entry := range rhoPiTable {
			v := A[entry.src]
			if entry.rot != 0 {
				B[entry.dst] = bits.RotateLeft64(v, entry.rot)
			} else {
				B[entry.dst] = v
			}
		}

		// Chi
		for y := 0; y < 5; y++ {
			row := y * 5
			for x := 0; x < 5; x++ {
				A[row+x] = B[row+x] ^ ((^B[row+((x+1)%5)]) & B[row+((x+2)%5)])
			}
		}

		// Iota
		A[0] ^= rc
	}
}

// DeepSeekHashV1 computes the SHA3-256 padding + Keccak-f[1600] rounds 1..23 hash.
func DeepSeekHashV1(data []byte) (string, error) {
	if len(data) >= sha3Rate {
		return "", errors.New("DeepSeekHashV1 input too long for single-block hashing")
	}

	var block [sha3Rate]byte
	copy(block[:], data)
	block[len(data)] = 0x06
	block[sha3Rate-1] |= 0x80

	var A [25]uint64
	for i := 0; i < 17; i++ {
		A[i] = binary.LittleEndian.Uint64(block[i*8 : (i+1)*8])
	}

	// Rounds 1..23 (skipping round 0)
	keccakF(&A, keccakRC[1:])

	var out [32]byte
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(out[i*8:(i+1)*8], A[i])
	}

	return hex.EncodeToString(out[:]), nil
}

// PowChallenge represents challenge data received from DeepSeek.
type PowChallenge struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	Difficulty int64  `json:"difficulty"`
	ExpireAt   int64  `json:"expire_at"`
	Signature  string `json:"signature"`
	TargetPath string `json:"target_path"`
}

// PowSolution represents the solved answer payload.
type PowSolution struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	Answer     int64  `json:"answer"`
	Signature  string `json:"signature"`
	TargetPath string `json:"target_path"`
}

// SolvePoW solves a DeepSeek PoW challenge using multi-core parallel search.
func SolvePoW(challenge PowChallenge) (string, error) {
	if challenge.Algorithm != "DeepSeekHashV1" {
		return "", fmt.Errorf("unsupported PoW algorithm: %s", challenge.Algorithm)
	}

	target := strings.ToLower(challenge.Challenge)
	prefix := fmt.Sprintf("%s_%d_", challenge.Salt, challenge.ExpireAt)
	prefixBytes := []byte(prefix)

	numWorkers := runtime.NumCPU()
	if numWorkers < 1 {
		numWorkers = 1
	}

	difficulty := challenge.Difficulty
	chunkSize := (difficulty + int64(numWorkers) - 1) / int64(numWorkers)

	var foundAnswer int64 = -1
	var found atomic.Bool
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		start := int64(w) * chunkSize
		end := start + chunkSize
		if end > difficulty {
			end = difficulty
		}
		if start >= difficulty {
			break
		}

		wg.Add(1)
		go func(s, e int64) {
			defer wg.Done()

			buf := make([]byte, len(prefixBytes)+24)
			copy(buf, prefixBytes)
			pLen := len(prefixBytes)

			for nonce := s; nonce < e; nonce++ {
				if found.Load() {
					return
				}

				nonceStr := strconv.FormatInt(nonce, 10)
				copy(buf[pLen:], nonceStr)
				inputLen := pLen + len(nonceStr)

				h, err := DeepSeekHashV1(buf[:inputLen])
				if err == nil && h == target {
					if found.CompareAndSwap(false, true) {
						foundAnswer = nonce
					}
					return
				}
			}
		}(start, end)
	}

	wg.Wait()

	if foundAnswer == -1 {
		return "", errors.New("PoW solution not found within difficulty limit")
	}

	sol := PowSolution{
		Algorithm:  challenge.Algorithm,
		Challenge:  challenge.Challenge,
		Salt:       challenge.Salt,
		Answer:     foundAnswer,
		Signature:  challenge.Signature,
		TargetPath: challenge.TargetPath,
	}

	jsonBytes, err := json.Marshal(sol)
	if err != nil {
		return "", fmt.Errorf("failed to marshal pow solution: %w", err)
	}

	return base64.StdEncoding.EncodeToString(jsonBytes), nil
}
