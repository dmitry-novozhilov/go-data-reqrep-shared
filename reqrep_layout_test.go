//go:build linux

package reqrep

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestLayoutMatchesC compiles gen/gen_layout.c, runs it, and verifies that
// every offset/size/constant matches the corresponding layout* Go constant.
// Skipped when gcc is not available.
func TestLayoutMatchesC(t *testing.T) {
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("gcc not found, skipping layout test")
	}

	bin, err := os.CreateTemp("", "reqrep_layout_test_*")
	if err != nil {
		t.Fatal(err)
	}
	bin.Close()
	defer os.Remove(bin.Name())

	cmd := exec.Command("gcc", "-D_GNU_SOURCE", "-o", bin.Name(), "gen/gen_layout.c", "-I", "gen/")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("gcc: %v", err)
	}

	out, err := exec.Command(bin.Name()).Output()
	if err != nil {
		t.Fatalf("gen binary: %v", err)
	}

	// Parse "Key          = value" lines from C binary output.
	cValues := map[string]uint64{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " = ", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 0, 64)
		if err != nil {
			t.Errorf("parse %q: %v", line, err)
			continue
		}
		cValues[key] = val
	}

	// Map from C output key to the corresponding Go layout* constant.
	goValues := map[string]uint64{
		// Value constants
		"Magic":          layoutMagic,
		"Version":        layoutVersion,
		"ModeStr":        layoutModeStr,
		"ModeInt":        layoutModeInt,
		"RespFree":       layoutRespFree,
		"RespAcquired":   layoutRespAcquired,
		"RespReady":      layoutRespReady,
		"Utf8Flag":       layoutUtf8Flag,
		"StrLenMask":     layoutStrLenMask,
		"MutexWriterBit": layoutMutexWriterBit,
		"MutexPidMask":   layoutMutexPidMask,
		"SpinLimit":      layoutSpinLimit,
		"LockTimeoutSec": layoutLockTimeoutSec,

		// Struct sizes
		"SizeofHeader":   layoutSizeofHeader,
		"SizeofReqSlot":  layoutSizeofReqSlot,
		"SizeofRespSlot": layoutSizeofRespSlot,

		// ReqRepHeader field offsets
		"HdrMagic":          layoutHdrMagic,
		"HdrVersion":        layoutHdrVersion,
		"HdrMode":           layoutHdrMode,
		"HdrReqCap":         layoutHdrReqCap,
		"HdrTotalSize":      layoutHdrTotalSize,
		"HdrReqSlotsOff":    layoutHdrReqSlotsOff,
		"HdrReqArenaOff":    layoutHdrReqArenaOff,
		"HdrReqArenaCap":    layoutHdrReqArenaCap,
		"HdrRespSlots":      layoutHdrRespSlots,
		"HdrRespDataMax":    layoutHdrRespDataMax,
		"HdrRespOff":        layoutHdrRespOff,
		"HdrRespStride":     layoutHdrRespStride,
		"HdrReqHead":        layoutHdrReqHead,
		"HdrRecvWaiters":    layoutHdrRecvWaiters,
		"HdrRecvFutex":      layoutHdrRecvFutex,
		"HdrReqTail":        layoutHdrReqTail,
		"HdrSendWaiters":    layoutHdrSendWaiters,
		"HdrSendFutex":      layoutHdrSendFutex,
		"HdrMutex":          layoutHdrMutex,
		"HdrMutexWaiters":   layoutHdrMutexWaiters,
		"HdrArenaWpos":      layoutHdrArenaWpos,
		"HdrArenaUsed":      layoutHdrArenaUsed,
		"HdrRespHint":       layoutHdrRespHint,
		"HdrStatRecoveries": layoutHdrStatRecoveries,
		"HdrSlotFutex":      layoutHdrSlotFutex,
		"HdrSlotWaiters":    layoutHdrSlotWaiters,
		"HdrStatRequests":   layoutHdrStatRequests,
		"HdrStatReplies":    layoutHdrStatReplies,
		"HdrStatSendFull":   layoutHdrStatSendFull,
		"HdrStatRecvEmpty":  layoutHdrStatRecvEmpty,

		// ReqSlot field offsets
		"ReqArenaOff":  layoutReqArenaOff,
		"ReqPackedLen": layoutReqPackedLen,
		"ReqArenaSkip": layoutReqArenaSkip,
		"ReqRespSlot":  layoutReqRespSlot,
		"ReqRespGen":   layoutReqRespGen,

		// RespSlotHeader field offsets
		"RespState":      layoutRespState,
		"RespWaiters":    layoutRespWaiters,
		"RespOwnerPid":   layoutRespOwnerPid,
		"RespLen":        layoutRespLen,
		"RespFlags":      layoutRespFlags,
		"RespGeneration": layoutRespGeneration,
		"RespDataStart":  layoutRespDataStart,
	}

	for key, goVal := range goValues {
		cVal, ok := cValues[key]
		if !ok {
			t.Errorf("key %q missing from C output", key)
			continue
		}
		if cVal != goVal {
			t.Errorf("%-20s C=%-6d Go=%d", key, cVal, goVal)
		}
	}

	// Ensure no C output keys are silently unchecked.
	for key := range cValues {
		if _, ok := goValues[key]; !ok {
			t.Logf("C output key %q has no corresponding Go constant check", key)
		}
	}
}
