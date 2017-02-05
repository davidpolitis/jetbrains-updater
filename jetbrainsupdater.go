package main

import (
	"log"
	"net/http"
	"bufio"
	"strings"
	"encoding/json"
	"os"
	"path/filepath"
	"io"
	"io/ioutil"
	"compress/gzip"
	"archive/tar"
	"errors"
	"gopkg.in/cheggaaa/pb.v1"
)

// Converts an octal value file permission from a string to os.FileMode.
// Borrowed from https://gist.github.com/doylecnn/6220a3cde39e286a64e4
func permFromString(perm_str string) (perm os.FileMode, err error) {
	err = nil
	if len(perm_str) != 4 {
		err =  errors.New("Permission is too long")
	}
	perm = perm | (os.ModeType & (os.FileMode(perm_str[0] - 48)))
	for i, c := range perm_str[1:] {
		if c < 48 || c > 55 {
			err =  errors.New("Permission isn't within valid range.")
		}

		if i == 0 {
			perm = os.ModePerm & (os.FileMode(c - 48))
		}
		perm = (perm << uint32(i + 1)) | (os.ModePerm & (os.FileMode(c - 48)))
	}
	return
}

// Downloads a file while simultaneously displaying the fraction and percentage complete
func downloadWithProgress(url, destination string) error {
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer out.Close()

	res, err := http.Get(url)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.ContentLength > 0 {
		bar := pb.New64(res.ContentLength).SetUnits(pb.U_BYTES)
		bar.Start()
		defer bar.Finish()

		reader := bar.NewProxyReader(res.Body)

		_, err = io.Copy(out, reader)
		if err != nil {
			return err
		}
	}

	return nil
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

		// form path without first directory
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
	// open config
	file, err1 := ioutil.ReadFile("./config.json")
	if err1 != nil {
		log.Fatal("Error opening config.json")
	}

	// config structure
	type Product struct {
		Name      string
		ID        string
		EAP       bool
		ParentDir string
		Dir       string
		Chmod     string
		Enabled   bool
	}

	// parse config
	var products []Product

	err2 := json.Unmarshal(file, &products)
	if err2 != nil {
		log.Fatal("Error parsing config.json")
	}

	// loop through and update each enabled product
	for p := range products {
		if products[p].Enabled == true && len(products[p].ParentDir) > 0 && len(products[p].Dir) > 0 {
			// construct latest version URL
			releasesUrl := "https://data.services.jetbrains.com/products/releases?code=" + products[p].ID + "&latest=true"
			if products[p].EAP {
				releasesUrl += "&type=eap"
			}

			// fetch JSON
			res, err3 := http.Get(releasesUrl)
			if err3 != nil {
				log.Fatal("Error downloading JSON for " + products[p].Name + ".", err3)
			}
			chars := bufio.NewReader(res.Body)

			// ensure JSON is valid
			jsonError, err4 := chars.Peek(8)
			if err4 == nil && string(jsonError) == `{"errors` {
				log.Fatal("JSON for " + products[p].Name + " contained an error. Are you sure the product exists?")
			}

			// discard bracket, quote and product ID
			chars.Discard(2 + len(products[p].ID))
			// create byte slice of needed length
			bytesLeft := chars.Buffered()
			sliceLen := 3 + bytesLeft
			jsonSlice := make([]byte, sliceLen, sliceLen)
			// prepend curly bracket, quote and static product ID to byte slice
			jsonSlice[0] = '{'
			jsonSlice[1] = '"'
			jsonSlice[2] = 'A'

			// append remaining characters to byte slice
			c, _ := chars.Peek(bytesLeft)
			for i := 0; i < len(c); i++ {
				jsonSlice[i + 3] = c[i]
			}

			type Release struct {
				A []struct {
					Build string

					Downloads struct {
						Windows struct {
							Link string
							ChecksumLink string
						}
						Mac struct {
							Link string
							ChecksumLink string
						}
						Linux struct {
							Link string
							ChecksumLink string
						}
					}
				}
			}

			var release = new(Release)
			err5 := json.Unmarshal(jsonSlice, &release)
			if err5 != nil {
				log.Fatal("Error parsing JSON for " + products[p].Name + ".", err5)
			}
			res.Body.Close()

			installDir := filepath.Join(products[p].ParentDir, products[p].Dir)
			// only update outdated software
			buildFile := filepath.Join(installDir, "build.txt")
			buildLine, err6 := ioutil.ReadFile(buildFile)
			if err6 == nil && buildLine != nil {
				currentBuild := strings.Split(string(buildLine), "-")

				if len(currentBuild) == 2 && currentBuild[1] >= release.A[0].Build {
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

			url := release.A[0].Downloads.Linux.Link

			nameAndBuild := products[p].Name + " " + release.A[0].Build

			// start file download
			log.Printf("Downloading %s (%s)...\n", nameAndBuild, url)
			err8 := downloadWithProgress(url, tmpFile)
			if err8 != nil {
				log.Fatalf("Error downloading %s (%s): %v", nameAndBuild, url, err8)
			}

			log.Printf("Downloaded %s.\n", nameAndBuild)

			// remove old installation
			log.Printf("Removing old %s if exists...\n", products[p].Name)
			os.RemoveAll(installDir)

			// make install dir with correct permissions
			if len(products[p].Chmod) > 0 {
				perm, err := permFromString(products[p].Chmod)
				if err != nil {
					log.Fatal(err)
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
