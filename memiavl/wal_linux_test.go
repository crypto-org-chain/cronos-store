//go:build linux

package memiavl

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// tryOpenPipeWriter returns nil until a load has the pipe open for reading.
func tryOpenPipeWriter(path string) *os.File {
	pipe, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	return pipe
}

// openPipeWriter waits for a load to open the pipe for reading, failing instead of hanging if none does.
func (s *ReadOnlyWALTestSuite) openPipeWriter(path string) *os.File {
	var pipe *os.File
	s.Require().Eventually(func() bool {
		pipe = tryOpenPipeWriter(path)
		return pipe != nil
	}, 10*time.Second, time.Millisecond, "no load opened %s", path)
	return pipe
}

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
	pipe := s.openPipeWriter(path)

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
	_, err = pipe.Write(content)
	s.Require().NoError(err)
	s.Require().NoError(pipe.Close())
	select {
	case err := <-stuck:
		s.Require().NoError(err)
	case <-time.After(10 * time.Second):
		s.FailNow("the stuck load never finished")
	}
	if blocked {
		otherErr = <-other
	}
	s.Require().NoError(otherErr)
	s.Require().False(blocked, "a read of another segment waited on a segment load")
}

func (s *ReadOnlyWALTestSuite) TestConcurrentReadsLoadASegmentOnce() {
	// a second load of the pipe would get no data, failing or blocking that reader.
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

	pipe := s.openPipeWriter(path)
	_, err = pipe.Write(content)
	s.Require().NoError(err)
	s.Require().NoError(pipe.Close())
	for range readers {
		select {
		case err := <-errs:
			s.Require().NoError(err)
		case <-time.After(10 * time.Second):
			s.FailNow("a reader is stuck on a second load of the pipe")
		}
	}
}

func (s *ReadOnlyWALTestSuite) TestSegmentLoadsInFlightAreCapped() {
	const perSegment = 5
	const segments = readOnlyWALCachedSegments + 1
	dir := s.writeWideWAL(perSegment*segments + 1)
	paths := make([]string, segments)
	contents := make([][]byte, segments)
	for n := range segments {
		paths[n] = filepath.Join(dir, fmt.Sprintf("%020d", perSegment*n+1))
		var err error
		contents[n], err = os.ReadFile(paths[n])
		s.Require().NoError(err)
		s.Require().NoError(os.Remove(paths[n]))
		s.Require().NoError(syscall.Mkfifo(paths[n], 0o600))
	}
	l, err := openReadOnlyWAL(dir)
	s.Require().NoError(err)
	defer func() { s.Require().NoError(l.Close()) }()

	errs := make(chan error, segments)
	for n := range segments {
		go func() {
			_, err := l.Read(uint64(perSegment*n + 1))
			errs <- err
		}()
	}

	writers := make([]*os.File, segments)
	opened := func() int {
		count := 0
		for n, path := range paths {
			if writers[n] == nil {
				writers[n] = tryOpenPipeWriter(path)
			}
			if writers[n] != nil {
				count++
			}
		}
		return count
	}
	s.Require().Eventually(func() bool { return opened() == readOnlyWALCachedSegments }, 10*time.Second, time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	s.Require().Equal(readOnlyWALCachedSegments, opened(), "a load beyond the cap must wait for a slot")

	// finishing the loads frees a slot for the last one.
	for n, w := range writers {
		if w != nil {
			_, err := w.Write(contents[n])
			s.Require().NoError(err)
			s.Require().NoError(w.Close())
			writers[n] = nil
			paths[n] = ""
		}
	}
	for n, path := range paths {
		if path == "" {
			continue
		}
		w := s.openPipeWriter(path)
		_, err := w.Write(contents[n])
		s.Require().NoError(err)
		s.Require().NoError(w.Close())
	}
	for range segments {
		select {
		case err := <-errs:
			s.Require().NoError(err)
		case <-time.After(10 * time.Second):
			s.FailNow("a load is stuck")
		}
	}
}
