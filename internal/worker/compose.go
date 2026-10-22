package worker

import (
	"fmt"

	"go.mitchellh.com/libghostty"
)

// Composed input (CapAttachCompose).
//
// A script, or the overlay's submit and switch, sends a whole prompt as one
// request rather than keystrokes. The worker, which holds the terminal,
// encodes it the way the program asked to receive input: the text as a
// paste, bracketed when the program turned bracketed paste on and otherwise
// with newlines as carriage returns and unsafe control bytes made spaces
// (libghostty's paste rules), then Enter as the program's keyboard mode
// wants it (CSI u under the kitty protocol's "report all keys" flag, modify
// other keys, or a plain carriage return). Empty text is Enter alone.

// compose encodes text, then Enter, for the program as the terminal's modes
// stand now. Run on the owner goroutine.
func (s *ownerState) compose(text []byte) ([]byte, error) {
	term := s.terminal.term
	var out []byte
	if len(text) > 0 {
		bracketed, err := term.Mode(libghostty.ModeBracketedPaste)
		if err != nil {
			return nil, err
		}
		if out, err = libghostty.PasteEncode(text, bracketed); err != nil {
			return nil, fmt.Errorf("failed to encode the paste: %w", err)
		}
	}
	enc, err := libghostty.NewKeyEncoder()
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	enc.SetOptFromTerminal(term)
	ev, err := libghostty.NewKeyEvent()
	if err != nil {
		return nil, err
	}
	defer ev.Close()
	ev.SetAction(libghostty.KeyActionPress)
	ev.SetKey(libghostty.KeyEnter)
	ev.SetUTF8("\r")
	enter, err := enc.Encode(ev)
	if err != nil {
		return nil, fmt.Errorf("failed to encode Enter: %w", err)
	}
	return append(out, enter...), nil
}
