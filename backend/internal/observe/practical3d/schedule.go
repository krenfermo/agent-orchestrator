package practical3d

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/chacha20"
)

// GenerateSchedule is task-paired-off-assisted-v1 of 06 §3.5.
func GenerateSchedule(r Randomization) ([]Position, error) {
	if r.PRNGAlgorithm != "ChaCha20-IETF" || r.PRNGVersion != "RFC8439-v1" ||
		r.RootSeedGeneration != "OS_CSPRNG_32_bytes_before_manifest_v1" ||
		r.TaskStreamDerivation != "sha256-task-domain-v1" ||
		r.ScheduleAlgorithm != "task-paired-off-assisted-v1" || r.ScheduleVersion != 1 ||
		r.OrientationBalance != "2_3_each_task" {
		return nil, fmt.Errorf("%w: randomization constants differ from the frozen protocol", ErrInvalidManifest)
	}
	seed, err := hex.DecodeString(r.SeedHex)
	if err != nil || len(seed) != 32 {
		return nil, fmt.Errorf("%w: seed_hex must be 32 bytes", ErrInvalidManifest)
	}
	out := make([]Position, 0, ExpectedPositions)
	positionIndex := 1
	for _, task := range []string{"A", "B", "C", "D"} {
		keyInput := append([]byte("AO-3D-PRACTICAL-TASK-v1"), seed...)
		keyInput = append(keyInput, []byte(task)...)
		key := sha256.Sum256(keyInput)
		words, err := newChaChaWords(key[:])
		if err != nil {
			return nil, err
		}
		k := 2
		if words.next()&1 == 1 {
			k = 3
		}
		permutation := []int{1, 2, 3, 4, 5}
		for i := len(permutation) - 1; i >= 1; i-- {
			mod := uint64(i + 1)
			limit := (uint64(1) << 32) / mod * mod
			var x uint32
			for {
				x = words.next()
				if uint64(x) < limit {
					break
				}
			}
			j := int(uint64(x) % mod) //nolint:gosec // mod <= 5, so the remainder fits in int.
			permutation[i], permutation[j] = permutation[j], permutation[i]
		}
		offFirst := map[int]bool{}
		for _, pair := range permutation[:k] {
			offFirst[pair] = true
		}
		for pair := 1; pair <= ExpectedN; pair++ {
			order := []Arm{ArmAssisted, ArmOff}
			if offFirst[pair] {
				order = []Arm{ArmOff, ArmAssisted}
			}
			for _, arm := range order {
				idInput := append([]byte("AO-3D-PRACTICAL-SAMPLE-v1"), seed...)
				idInput = append(idInput, []byte(task)...)
				idInput = append(idInput, byte(pair))
				idInput = append(idInput, []byte(arm)...)
				id := sha256.Sum256(idInput)
				out = append(out, Position{PositionIndex: positionIndex, TaskID: task, PairIndex: pair, PairOrder: append([]Arm(nil), order...), Arm: arm, SampleID: hex.EncodeToString(id[:])})
				positionIndex++
			}
		}
	}
	return out, nil
}

type chachaWords struct {
	cipher *chacha20.Cipher
	buf    [4]byte
}

func newChaChaWords(key []byte) (*chachaWords, error) {
	c, err := chacha20.NewUnauthenticatedCipher(key, make([]byte, chacha20.NonceSize))
	if err != nil {
		return nil, err
	}
	return &chachaWords{cipher: c}, nil
}

func (w *chachaWords) next() uint32 {
	clear(w.buf[:])
	w.cipher.XORKeyStream(w.buf[:], w.buf[:])
	return binary.LittleEndian.Uint32(w.buf[:])
}
