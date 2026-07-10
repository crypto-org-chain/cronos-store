package memiavl

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// once the import goroutine exits on an error, further AddNode calls must
// surface it rather than block on a full channel.
func TestImportErrorDoesNotHang(t *testing.T) {
	importer, err := NewMultiTreeImporter(t.TempDir(), 1)
	require.NoError(t, err)
	defer importer.Close()
	require.NoError(t, importer.AddTree("test"))

	done := make(chan error, 1)
	go func() {
		var e error
		for i := 0; i < NodeChannelBuffer*2; i++ {
			if e = importer.AddNode(&ExportNode{Height: 1, Version: 1, Key: []byte("k")}); e != nil {
				break
			}
		}
		done <- e
	}()

	select {
	case e := <-done:
		require.Error(t, e, "import error should be surfaced to the producer")
	case <-time.After(30 * time.Second):
		t.Fatal("AddNode hung: import error was not surfaced")
	}
}
