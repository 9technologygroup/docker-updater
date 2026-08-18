//go:build linux

package cli

import "golang.org/x/sys/unix"

type termState = unix.Termios

func getTermState(fd int) (termState, error) {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return termState{}, err
	}
	return *t, nil
}

func setTermState(fd int, s termState) error {
	return unix.IoctlSetTermios(fd, unix.TCSETS, &s)
}

func echoOff(s termState) termState {
	s.Lflag &^= unix.ECHO
	return s
}
