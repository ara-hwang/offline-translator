//go:build windows

// Package winapi holds the small pieces of Win32 the desktop needs that
// mygo does not wrap: capturing the selected text of the focused
// application by synthesizing a copy shortcut into it and reading the
// clipboard, then putting the user's previous clipboard content back.
package winapi

import (
	"encoding/binary"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	user32                = syscall.NewLazyDLL("user32.dll")
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procSendInput         = user32.NewProc("SendInput")
	procOpenClipboard     = user32.NewProc("OpenClipboard")
	procCloseClipboard    = user32.NewProc("CloseClipboard")
	procGetClipboardData  = user32.NewProc("GetClipboardData")
	procSetClipboardData  = user32.NewProc("SetClipboardData")
	procEmptyClipboard    = user32.NewProc("EmptyClipboard")
	procClipboardSequence = user32.NewProc("GetClipboardSequenceNumber")
	procGlobalAlloc       = kernel32.NewProc("GlobalAlloc")
	procGlobalLock        = kernel32.NewProc("GlobalLock")
	procGlobalUnlock      = kernel32.NewProc("GlobalUnlock")
	procGlobalSize        = kernel32.NewProc("GlobalSize")
	procGlobalFree        = kernel32.NewProc("GlobalFree")
)

const (
	inputKeyboard  = 1
	keyeventfKeyup = 0x0002
	vkControl      = 0x11
	vkShift        = 0x10
	vkMenu         = 0x12
	vkLWin         = 0x5B
	vkRWin         = 0x5C
	vkC            = 0x43
	cfUnicodeText  = 13
	gmemMoveable   = 0x0002

	openAttempts = 5
	openRetry    = 10 * time.Millisecond
	copyTimeout  = 250 * time.Millisecond
	pollInterval = 16 * time.Millisecond
)

// keybdInput mirrors KEYBDINPUT.
type keybdInput struct {
	WVK   uint16
	Scan  uint16
	Flags uint32
	Time  uint32
	Extra uintptr
}

// inputUnion mirrors INPUT on amd64 (40 bytes total, the mouse variant is
// the largest union member).
type inputUnion struct {
	Type uint32
	_    [4]byte
	Kbd  keybdInput
	_    [8]byte
}

var inputSize = uint32(unsafe.Sizeof(inputUnion{}))

// native turns an address of memory not managed by Go into a pointer.
func native(addr uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&addr)) }

// CaptureSelected copies the selection of the focused application with a
// synthetic Ctrl+C and returns its text; the user's previous clipboard text
// (when there was one) is written back afterwards.
func CaptureSelected() (string, error) {
	previous := readClipboardText()
	baseline := clipboardSequence()
	injectCopyChord()

	deadline := time.Now().Add(copyTimeout)
	for {
		if clipboardSequence() != baseline {
			if text := readClipboardText(); text != "" {
				_ = writeClipboardText(previous)
				return validateCapture(text)
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(pollInterval)
	}
	_ = writeClipboardText(previous)
	return "", fmt.Errorf("선택한 텍스트를 복사하지 못했습니다. 텍스트를 선택한 상태에서 다시 시도해 주세요.")
}

// validateCapture applies the same non-empty rule the manual translation
// path enforces.
func validateCapture(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("선택한 텍스트가 없습니다.")
	}
	return text, nil
}

// sendKey sends one key press (`down`) or release.
func sendKey(vk uint16, down bool) {
	var in inputUnion
	in.Type = inputKeyboard
	in.Kbd = keybdInput{WVK: vk}
	if !down {
		in.Kbd.Flags = keyeventfKeyup
	}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), uintptr(inputSize))
}

// injectCopyChord sends the synthetic copy chord after resetting the
// logical modifiers still physically held from the hotkey press itself:
// with Shift still down, the chord would arrive as Shift+Ctrl+C, which
// most applications do not treat as copy.
func injectCopyChord() {
	sendKey(vkShift, false)
	sendKey(vkControl, false)
	sendKey(vkMenu, false)
	sendKey(vkLWin, false)
	sendKey(vkRWin, false)
	sendKey(vkControl, true)
	sendKey(vkC, true)
	sendKey(vkC, false)
	sendKey(vkControl, false)
}

// clipboardSequence is the system clipboard sequence number, incremented on
// every successful clipboard write.
func clipboardSequence() uint32 {
	r, _, _ := procClipboardSequence.Call()
	return uint32(r)
}

// readClipboardText reads the current CF_UNICODETEXT clipboard content, or
// "" when the clipboard is empty or unreadable. OpenClipboard is retried
// while another process owns it.
func readClipboardText() string {
	var r uintptr
	for i := 0; i < openAttempts; i++ {
		r, _, _ = procOpenClipboard.Call(0)
		if r != 0 {
			break
		}
		time.Sleep(openRetry)
	}
	if r == 0 {
		return ""
	}
	defer procCloseClipboard.Call()
	text := ""
	if d, _, _ := procGetClipboardData.Call(cfUnicodeText); d != 0 {
		text = readGlobalString(d)
	}
	return text
}

// readGlobalString copies the NUL-terminated UTF-16 content of a handle.
func readGlobalString(handle uintptr) string {
	p, _, _ := procGlobalLock.Call(handle)
	if p == 0 {
		return ""
	}
	defer procGlobalUnlock.Call(handle)
	size, _, _ := procGlobalSize.Call(handle)
	words := unsafe.Slice((*uint16)(native(p)), size/2)
	end := 0
	for end < len(words) && words[end] != 0 {
		end++
	}
	return string(utf16.Decode(words[:end]))
}

// CopyText copies text to the system clipboard, leaving it unchanged for empty text.
func CopyText(text string) error {
	return writeClipboardText(text)
}

// writeClipboardText replaces the clipboard text; empty means "leave the
// clipboard alone", so the text the copy just produced survives when there
// is nothing to restore.
func writeClipboardText(text string) error {
	if text == "" {
		return nil
	}
	wide := utf16.Encode([]rune(text))
	packed := make([]byte, 2*(len(wide)+1))
	for i, w := range wide {
		binary.LittleEndian.PutUint16(packed[2*i:], w)
	}
	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		return fmt.Errorf("클립보드를 열 수 없습니다.")
	}
	defer procCloseClipboard.Call()
	if r, _, _ := procEmptyClipboard.Call(); r == 0 {
		return fmt.Errorf("클립보드를 비울 수 없습니다.")
	}
	handle, _, _ := procGlobalAlloc.Call(gmemMoveable, uintptr(len(packed)))
	if handle == 0 {
		return fmt.Errorf("클립보드 메모리를 할당하지 못했습니다.")
	}
	p, _, _ := procGlobalLock.Call(handle)
	if p == 0 {
		procGlobalFree.Call(handle)
		return fmt.Errorf("클립보드 메모리를 잠그지 못했습니다.")
	}
	copy(unsafe.Slice((*byte)(native(p)), len(packed)), packed)
	procGlobalUnlock.Call(handle)
	if r, _, _ := procSetClipboardData.Call(cfUnicodeText, handle); r == 0 {
		procGlobalFree.Call(handle)
		return fmt.Errorf("클립보드에 쓰지 못했습니다.")
	}
	// Ownership of `handle` moved to the clipboard; do not free it.
	return nil
}
