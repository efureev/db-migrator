package db

import (
	"testing"
)

func Test_Status(t *testing.T) {
	InitConfigForTest()
	size, dbStr := status()

	if size == `` {
		t.Fatal("Size should not be empty")
	}
	if dbStr == `` {
		t.Fatal("dbStr should not be empty")
	}
}
