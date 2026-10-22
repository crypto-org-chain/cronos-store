//go:build unix

package memiavl

import (
	"fmt"
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
	var (
		blocked  bool
		otherErr error
	)
	select {
	case otherErr = <-other:
	case <-time.After(time.Second):
		blocked = true
	}

	// unblock the stuck load before any assertion can end the test.
	pipe, err := os.OpenFile(path, os.O_WRONLY, 0)
	s.Require().NoError(err)
	_, err = pipe.Write(content)
	s.Require().NoError(err)
	s.Require().NoError(pipe.Close())
	s.Require().NoError(<-stuck)
	if blocked {
		otherErr = <-other
	}
	s.Require().NoError(otherErr)
	s.Require().False(blocked, "a read of another segment waited on a segment load")
}

func (s *ReadOnlyWALTestSuite) TestConcurrentReadsLoadASegmentOnce() {
	// a second open of the pipe would split its one write between two loads.
	path := s.segmentPath(6, "")
	content, err := os.ReadFile(path)
	s.Require().NoError(err)
	s.Require().NoError(os.Remove(path))
	s.Require().NoError(syscall.Mkfifo(path, 0o600))

	l, err := openReadOnlyWAL(s.dir)
	s.Require().NoError(err)
	defer func() { s.Require().NoError(l.Close()) }()

	const readers = 2
	errs := make(chan error, readers)
	for range readers {
		go func() {
			data, err := l.Read(7)
			if err == nil && string(data) != string(roWALEntry(7)) {
				err = fmt.Errorf("got %q", data)
			}
			errs <- err
		}()
	}
	// give both readers time to reach the load.
	time.Sleep(100 * time.Millisecond)

	pipe, err := os.OpenFile(path, os.O_WRONLY, 0)
	s.Require().NoError(err)
	_, err = pipe.Write(content)
	s.Require().NoError(err)
	s.Require().NoError(pipe.Close())
	for range readers {
		s.Require().NoError(<-errs)
	}
}
