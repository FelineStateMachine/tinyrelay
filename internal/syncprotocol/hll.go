package syncprotocol

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

const hllRegisters = 256

// HLL is the NIP-45 256-register sketch used to merge counts from relays.
type HLL struct {
	offset int
	regs   [hllRegisters]uint8
}

func NewHLL(offset int) *HLL {
	if offset < 0 || offset >= 32 {
		return &HLL{}
	}
	return &HLL{offset: offset}
}

func HLLFilterOffset(f event.Filter) (int, bool) {
	if len(f.Tags) != 1 {
		return 0, false
	}
	for _, values := range f.Tags {
		if len(values) == 0 {
			return 0, false
		}
		value := values[0]
		parts := strings.Split(value, ":")
		if len(parts) == 3 && len(parts[1]) == 64 {
			value = parts[1]
		}
		if len(value) != 64 || !isLowerHex(value) {
			sum := sha256.Sum256([]byte(value))
			value = hex.EncodeToString(sum[:])
		}
		digit, err := strconv.ParseUint(string(value[32]), 16, 4)
		if err != nil {
			return 0, false
		}
		return int(digit) + 8, true
	}
	return 0, false
}

func (h *HLL) Add(pubkeyHex string) error {
	if len(pubkeyHex) != 64 || !isHexCharacters(pubkeyHex) {
		return fmt.Errorf("invalid pubkey: expected 32-byte hex")
	}
	key, err := hex.DecodeString(pubkeyHex)
	if err != nil {
		return fmt.Errorf("decode pubkey: %w", err)
	}
	register := key[h.offset]
	zeros := 0
	for i := h.offset + 1; i < len(key); i++ {
		if key[i] == 0 {
			zeros += 8
			continue
		}
		zeros += leadingZeros8(key[i])
		break
	}
	if value := uint8(zeros + 1); value > h.regs[register] {
		h.regs[register] = value
	}
	return nil
}

func (h *HLL) Hex() string { return hex.EncodeToString(h.regs[:]) }

func (h *HLL) MergeHex(encoded string) error {
	if len(encoded) != hllRegisters*2 {
		return fmt.Errorf("invalid HLL: expected %d hex characters", hllRegisters*2)
	}
	registers, err := hex.DecodeString(encoded)
	if err != nil || len(registers) != hllRegisters {
		return fmt.Errorf("invalid HLL registers")
	}
	for i, value := range registers {
		if value > h.regs[i] {
			h.regs[i] = value
		}
	}
	return nil
}

func leadingZeros8(value byte) int {
	zeros := 0
	for bit := byte(0x80); value&bit == 0; bit >>= 1 {
		zeros++
	}
	return zeros
}

func isLowerHex(value string) bool {
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func isHexCharacters(value string) bool {
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}
