package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var (
	clean      bool
	extensions []string
)

var harvestCmd = &cobra.Command{
	Use:   "harvest",
	Short: "harvest is a tool to harvest files",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("missing directory argument")
		}

		workDir := args[0]

		// Check if the directory exists
		info, err := os.Stat(workDir)
		if err != nil {
			return fmt.Errorf("failed to stat directory: %v", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", workDir)
		}

		fmt.Println("Harvesting files in directory: ", workDir)
		fmt.Println("Harvesting files with extensions: ", extensions)

		deletingDirs := make(map[string]bool)

		err = filepath.WalkDir(workDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if !d.IsDir() && filepath.Dir(path) != workDir {
				for _, ext := range extensions {
					if strings.EqualFold(filepath.Ext(path), "."+ext) {
						destPath := filepath.Join(workDir, d.Name())

						// check if the file already exists
						if _, err := os.Stat(destPath); err == nil {
							base := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
							ext := filepath.Ext(d.Name())
							i := 1
							for {
								newName := fmt.Sprintf("%s_%d%s", base, i, ext)
								destPath = filepath.Join(workDir, newName)
								if _, err := os.Stat(destPath); os.IsNotExist(err) {
									break
								}
								i++
							}
						}

						// extract the directory
						dir := filepath.Dir(path)
						deletingDirs[dir] = true

						// moving the file
						fmt.Printf("Moving file %s to %s\n", path, destPath)
						err := os.Rename(path, destPath)
						if err != nil {
							return fmt.Errorf("failed to move file %s: %v", path, err)
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("error harvesting files: %v", err)
		}

		if clean {
			for deletingDir := range deletingDirs {
				fmt.Println("Deleting directory: ", deletingDir)

				err := os.RemoveAll(deletingDir)
				if err != nil {
					fmt.Printf("Warning: failed to delete directory %s: %v\n", deletingDir, err)
				}
			}
		}

		return nil
	},
}

func init() {
	harvestCmd.Flags().StringSliceVar(&extensions, "ext",
		[]string{"avi", "mp4", "mkv", "wmv", "smi", "srt"},
		"List of extensions to harvest")
	harvestCmd.Flags().BoolVar(&clean, "clean", false, "Clean up empty directories")
}
