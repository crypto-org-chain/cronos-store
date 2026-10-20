//go:build unix

package memiavl

import (
	"os"
	"syscall"
	"time"
)

func (s *ReadOnlyWALTestSuite) TestSegmentLoadDoesNotBlockOtherReads() {
	// a named pipe holds the segment's load in os.ReadFile until the test writes it.
	path := s.segmentPath(6, "")
	content, err := os.ReadFile(path)
	s.Require().NoError(err)
	s.Require().NoError(os.Remove(path))
	s.Require().NoError(syscall.Mkfifo(path, 0o600))

	l, err := openReadOnlyWAL(s.dir)
	s.Require().NoError(err)
	defer func() { s.Require().NoError(l.Close()) }()

	stuck := make(chan error, 1)
	go func() {
		data, err := l.Read(6)
		if err == nil && string(data) != string(roWALEntry(6)) {
			err = os.ErrInvalid
		}
		stuck <- err
	}()
	// give the stuck load time to block on the pipe.
	time.Sleep(100 * time.Millisecond)

	other := make(chan error, 1)
	go func() {
		_, err := l.Read(1)
		other <- err
	}()
	var blocked bool
	select {
	case err := <-other:
		s.Require().NoError(err)
	case <-time.After(time.Second):
		blocked = true
	}

	pipe, err := os.OpenFile(path, os.O_WRONLY, 0)
	s.Require().NoError(err)
	_, err = pipe.Write(content)
	s.Require().NoError(err)
	s.Require().NoError(pipe.Close())
	s.Require().NoError(<-stuck)
	if blocked {
		s.Require().NoError(<-other)
	}
	s.Require().False(blocked, "a read of another segment waited on a segment load")
}
