//go:build darwin

package cli

import "golang.org/x/sys/unix"

type termState = unix.Termios

func getTermState(fd int) (termState, error) {
	t, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return termState{}, err
	}
	return *t, nil
}

func setTermState(fd int, s termState) error {
	return unix.IoctlSetTermios(fd, unix.TIOCSETA, &s)
}

func echoOff(s termState) termState {
	s.Lflag &^= unix.ECHO
	return s
}

func terminalWidth(fd int) int {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 {
		return 0
	}
	return int(ws.Col)
}
