package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var extensions []string

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

		err = filepath.WalkDir(workDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && filepath.Dir(path) != workDir {
				for _, ext := range extensions {
					if filepath.Ext(path) == "."+ext {
						destPath := filepath.Join(workDir, d.Name())
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

		return nil
	},
}

func init() {
	harvestCmd.Flags().StringSliceVar(&extensions, "ext",
		[]string{"avi", "mp4", "mkv", "wmv"},
		"List of extensions to harvest")
}
