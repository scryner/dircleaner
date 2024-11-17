package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestHarvestCmd(t *testing.T) {
	// Create a temporary directory
	tempDir, err := os.MkdirTemp("", "harvest_test")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create subdirectories and files
	subDir := filepath.Join(tempDir, "subdir")
	err = os.Mkdir(subDir, 0755)
	if err != nil {
		t.Fatalf("Failed to create subdir: %v", err)
	}

	files := []string{
		filepath.Join(subDir, "file1.avi"),
		filepath.Join(subDir, "file2.mp4"),
		filepath.Join(subDir, "file3.txt"),
		filepath.Join(subDir, "file4.AVI"),
		filepath.Join(subDir, "file5.MP4"),
	}

	for _, file := range files {
		err = os.WriteFile(file, []byte("test content"), 0644)
		if err != nil {
			t.Fatalf("Failed to create file %s: %v", file, err)
		}
	}

	// Run the harvest command
	cmd := &cobra.Command{}
	cmd.SetArgs([]string{tempDir})
	err = harvestCmd.RunE(cmd, []string{tempDir})
	if err != nil {
		t.Fatalf("harvestCmd failed: %v", err)
	}

	// Check if the files are moved correctly
	expectedFiles := []string{
		filepath.Join(tempDir, "file1.avi"),
		filepath.Join(tempDir, "file2.mp4"),
		filepath.Join(tempDir, "file4.AVI"),
		filepath.Join(tempDir, "file5.MP4"),
	}

	for _, file := range expectedFiles {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			t.Errorf("Expected file %s to be moved, but it does not exist", file)
		}
	}

	// Check if the non-matching file is not moved
	nonExpectedFile := filepath.Join(tempDir, "file3.txt")
	if _, err := os.Stat(nonExpectedFile); err == nil {
		t.Errorf("Expected file %s to not be moved, but it exists", nonExpectedFile)
	}
}
