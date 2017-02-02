package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"encoding/json"
	"time"
	"bytes"
	"os"
	"path/filepath"
	"io"
	"io/ioutil"
	"compress/gzip"
	"archive/tar"
	"errors"
	"github.com/antchfx/xquery/xml"
	"github.com/cavaliercoder/grab"
)

// Converts an octal value file permission from a string to os.FileMode.
// Borrowed from https://gist.github.com/doylecnn/6220a3cde39e286a64e4
func permFromString(perm_str string) (perm os.FileMode, err error) {
	err = nil
	if len(perm_str) != 4 {
		err = fmt.Errorf("too long")
	}
	perm = perm | (os.ModeType & (os.FileMode(perm_str[0] - 48)))
	for i, c := range perm_str[1:] {
		if c < 48 || c > 55 {
			err = fmt.Errorf("out of range")
		}

		if i == 0 {
			perm = os.ModePerm & (os.FileMode(c - 48))
		}
		perm = (perm << uint32(i + 1)) | (os.ModePerm & (os.FileMode(c - 48)))
	}
	return
}

// Extracts the contents of the first directory in a .tar.gz file to the specified destination.
// Adapted from http://blog.ralch.com/tutorial/golang-working-with-tar-and-gzip/
func untargz(source, destination string) error {
	// open path file
	reader, err := os.Open(source)
	if err != nil {
		return err
	}
	defer reader.Close()

	// read file into gzip reader
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return err
	}
	defer gzipReader.Close()

	// read gzip reader into tar reader
	tarReader := tar.NewReader(gzipReader)

	firstSeparator := -1

	// create each file/directory
	for {
		header, err := tarReader.Next()

		switch {
			// if no more files are found return
			case err == io.EOF:
				return nil

			// return any other error
			case err != nil:
				return err

			// if the header is nil, just skip it (not sure how this happens)
			case header == nil:
				continue

			// skip first directory and save name in variable
			case firstSeparator == -1:
				firstSeparator = strings.Index(header.Name, string(os.PathSeparator))
		}

		// form path filepath without first directory
		afterFirstDir := header.Name[firstSeparator + 1:]
		path := filepath.Join(destination, afterFirstDir)
		info := header.FileInfo()

		// check the file type
		switch header.Typeflag {
			// if it's a directory, create it
			case tar.TypeDir:
				if _, err := os.Stat(path); err != nil {
					if err := os.MkdirAll(path, info.Mode()); err != nil {
						return err
					}
				}

			// if it's a file, create parent directories and it
			case tar.TypeReg:
				// create parent directories if they don't exist
				parentDir := filepath.Dir(path)
				if _, err := os.Stat(parentDir); err != nil {
					if err := os.MkdirAll(parentDir, 0755); err != nil {
						return err
					}
				}

				file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
				if err != nil {
					return err
				}

				// copy over contents
				if _, err := io.Copy(file, tarReader); err != nil {
					return err
				}

				file.Close()

			default:
				return errors.New("Couldn't determine file-type.")
		}
	}
}

func main() {
	// fetch XML
	res, err1 := http.Get("https://www.jetbrains.com/updates/updates.xml")
	if err1 != nil {
		log.Fatal("Error getting XML. ", err1)
	}
	defer res.Body.Close()

	body, err2 := ioutil.ReadAll(res.Body)
	if err2 != nil {
		log.Fatal("Error reading XML. ", err2)
	}

	// add missing XML declaration
	body = append([]byte(`<?xml version="1.0" encoding="UTF-8"?>`), body...)

	// parse XML
	root, err3 := xmlquery.ParseXML(bytes.NewReader(body))
	if err3 != nil {
		log.Fatal("Error parsing XML. ", err3)
	}

	// open config
	file, err4 := ioutil.ReadFile("./config.json")
	if err4 != nil {
		log.Fatal("Error opening config.json")
	}

	// config structure
	type Product struct {
		Name      string
		Url       string
		ParentDir string
		Dir       string
		Chmod     string
		Enabled   bool
	}

	// parse config
	var products []Product

	err5 := json.Unmarshal(file, &products)
	if err5 != nil {
		log.Fatal("Error parsing config.json")
	}

	// loop through and update each enabled product
	for p := range products {
		if products[p].Enabled == true && len(products[p].ParentDir) > 0 && len(products[p].Dir) > 0 {
			// find all builds for product
			builds := xmlquery.Find(root, "//product[@name='" + products[p].Name + "']/channel[@status='eap' or @status='EAP']/build")

			var build string

			// select newest build
			for b := range builds {
				fullNumber := builds[b].SelectAttr("fullNumber")

				if fullNumber > build {
					build = fullNumber
				}
			}

			// unsupported xpath 1.0 query version of the code above
			//build := xmlquery.Find(root, "//product[@name='" + products[p].Name + "']/channel[@status='eap' or @status='EAP']/build[not(@fullNumber < preceding-sibling::build/@fullNumber) and not(@fullNumber < following-sibling::build/@fullNumber)]/@fullNumber")

			installDir := filepath.Join(products[p].ParentDir, products[p].Dir)

			// only update outdated software
			buildFile := filepath.Join(installDir, "build.txt")
			buildLine, err6 := ioutil.ReadFile(buildFile)
			if err6 == nil && buildLine != nil {
				currentBuild := strings.Split(string(buildLine), "-")

				if len(currentBuild) == 2 && currentBuild[1] >= build {
					log.Printf("%s is already up-to-date. Continuing...\n", products[p].Name)

					continue
				}
			}

			// create temporary directory to download and extract installation to
			tmpDir, err7 := ioutil.TempDir("", products[p].Dir + "-")
			if err7 != nil {
				log.Fatal("Error creating temporary directory.")
			}

			tmpFile := filepath.Join(tmpDir, "installation.tar.gz")

			url := fmt.Sprintf(products[p].Url, build)

			nameAndBuild := products[p].Name + " " + build

			// start file download
			log.Printf("Downloading %s (%s)...\n", nameAndBuild, url)
			respch, err8 := grab.GetAsync(tmpFile, url)
			if err8 != nil {
				log.Fatalf("Error downloading %s (%s): %v", nameAndBuild, url, err8)
			}

			// block until HTTP/1.1 GET response is received
			log.Printf("Initialising download...\n")
			resp := <-respch

			// print progress until transfer is complete
			for !resp.IsComplete() {
				log.Printf("\033[1AProgress %d / %d bytes (%d%%)\033[K\n", resp.BytesTransferred(), resp.Size, int(100 * resp.Progress()))
				time.Sleep(200 * time.Millisecond)
			}

			// clear progress line
			log.Printf("\033[1A\033[K")

			// check for errors
			if resp.Error != nil {
				log.Fatalf("Error downloading %s (%s): %v", nameAndBuild, url, resp.Error)
			}

			log.Printf("Successfully downloaded %s.\n", nameAndBuild)

			// remove old installation
			log.Printf("Removing old %s if exists...\n", products[p].Name)
			os.RemoveAll(installDir)

			// make install dir with correct permissions
			if len(products[p].Chmod) > 0 {
				perm, err := permFromString("0777")
				if err != nil {
					log.Printf("Invalid permission string: %s\n", products[p].Chmod)
				}

				os.Mkdir(installDir, perm)
			}

			// extract installation
			log.Printf("Extracting files for %s\n", nameAndBuild)
			err9 := untargz(tmpFile, installDir)
			if err9 != nil {
				log.Fatal(err9)
			}

			// remove temporary, archive directory
			os.RemoveAll(tmpDir)

			// finish
			log.Printf("%s was installed!\n", nameAndBuild)
		}
	}
}
