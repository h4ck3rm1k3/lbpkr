package yum

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gonuts/logger"
)

// global registry of known backends
var g_backends = make(map[string]func(repo *Repository) (Backend, error))

// NewBackend returns a new backend of type "backend"
func NewBackend(backend string, repo *Repository) (Backend, error) {
	factory, ok := g_backends[backend]
	if !ok {
		return nil, fmt.Errorf("yum: no such backend [%s]", backend)
	}
	return factory(repo)
}

// Backend queries a YUM DB repository
type Backend interface {

	// YumDataType returns the ID for the data type as used in the repomd.xml file
	YumDataType() string

	// Download the DB from server
	GetLatestDB(url string) error

	// Check whether the DB is there
	HasDB() bool

	// Load loads the DB
	LoadDB() error

	// FindLatestMatchingName locats a package by name, returns the latest available version.
	FindLatestMatchingName(name, version, release string) (*Package, error)

	// FindLatestMatchingRequire locates a package providing a given functionality.
	FindLatestMatchingRequire(requirement string) (*Package, error)

	// GetPackages returns all the packages known by a YUM repository
	GetPackages() []*Package
}

// Repository represents a YUM repository with all associated metadata.
type Repository struct {
	msg            *logger.Logger
	Name           string
	RepoUrl        string
	RepoMdUrl      string
	LocalRepoMdXml string
	CacheDir       string
	Backends       []string
	Backend        Backend
}

// NewRepository create a new Repository with name and from url.
func NewRepository(name, url, cachedir string, backends []string, setupBackend, checkForUpdates bool) (*Repository, error) {

	repo := Repository{
		msg:            logger.NewLogger("yum", logger.INFO, os.Stdout),
		Name:           name,
		RepoUrl:        url,
		RepoMdUrl:      url + "/repodata/repomd.xml",
		LocalRepoMdXml: filepath.Join(cachedir, "repomd.xml"),
		CacheDir:       cachedir,
		Backends:       make([]string, len(backends)),
	}
	copy(repo.Backends, backends)

	err := os.MkdirAll(cachedir, 0644)
	if err != nil {
		return nil, err
	}

	// load appropriate backend if requested
	if setupBackend {
		if checkForUpdates {
			err = repo.setupBackendFromRemote()
			if err != nil {
				return nil, err
			}
		} else {
			err = repo.setupBackendFromLocal()
			if err != nil {
				return nil, err
			}
		}
	}
	return &repo, err
}

// FindLatestMatchingName locats a package by name, returns the latest available version.
func (repo *Repository) FindLatestMatchingName(name, version, release string) (*Package, error) {
	return repo.Backend.FindLatestMatchingName(name, version, release)
}

// FindLatestMatchingRequire locates a package providing a given functionality.
func (repo *Repository) FindLatestMatchingRequire(requirement string) (*Package, error) {
	return repo.Backend.FindLatestMatchingRequire(requirement)
}

// GetPackages returns all the packages known by a YUM repository
func (repo *Repository) GetPackages() []*Package {
	return repo.Backend.GetPackages()
}

// setupBackendFromRemote checks which backend should be used and updates the DB files.
func (repo *Repository) setupBackendFromRemote() error {
	repo.msg.Infof("setupBackendFromRemote...\n")
	var err error
	var backend Backend
	// get repo metadata with list of available files
	remotedata, err := repo.remoteMetadata()
	if err != nil {
		return err
	}

	remotemd, err := repo.checkRepoMD(remotedata)
	if err != nil {
		return err
	}

	localdata, err := repo.localMetadata()
	if err != nil {
		return err
	}

	localmd, err := repo.checkRepoMD(localdata)
	if err != nil {
		return err
	}

	for _, bname := range repo.Backends {
		repo.msg.Infof("checking availability of backend [%s]\n", bname)
		ba, err := NewBackend(bname, repo)
		if err != nil {
			continue
		}

		rrepomd, ok := remotemd[ba.YumDataType()]
		if !ok {
			repo.msg.Warnf("remote repository does not provide [%s] DB\n", bname)
			continue
		}

		// a priori a match
		backend = ba
		repo.Backend = backend

		lrepomd, ok := localmd[ba.YumDataType()]
		if !ok {
			// doesn't matter, we download the DB in any case
		}

		if !repo.Backend.HasDB() || rrepomd.Timestamp.After(lrepomd.Timestamp) {
			// we need to update the DB
			url := repo.RepoUrl + "/" + rrepomd.Location
			repo.msg.Infof("updating the RPM database for %s\n", bname)
			err = repo.Backend.GetLatestDB(url)
			if err != nil {
				repo.msg.Warnf("problem updating RPM database for backend [%s]: %v\n", bname, err)
				err = nil
				backend = nil
				repo.Backend = nil
				continue
			}
			// save metadata to local repomd file
			err = ioutil.WriteFile(repo.LocalRepoMdXml, remotedata, 0644)
			if err != nil {
				repo.msg.Warnf("problem updating local repomd.xml file for backend [%s]: %v\n", bname, err)
				err = nil
				backend = nil
				repo.Backend = nil
				continue
			}
		}

		// load data necessary for the backend
		err = repo.Backend.LoadDB()
		if err != nil {
			repo.msg.Warnf("problem loading data for backend [%s]: %v\n", bname, err)
			err = nil
			backend = nil
			repo.Backend = nil
			continue
		}

		// stop at first one found
		break
	}

	if backend == nil {
		repo.msg.Errorf("No valid backend found\n")
		return fmt.Errorf("No valid backend found")
	}

	repo.msg.Infof("repository [%s] - chosen backend [%T]\n", repo.Name, repo.Backend)
	return err
}

func (repo *Repository) setupBackendFromLocal() error {
	repo.msg.Infof("setupBackendFromLocal...\n")
	var err error
	data, err := repo.localMetadata()
	if err != nil {
		return err
	}

	md, err := repo.checkRepoMD(data)
	if err != nil {
		return err
	}

	var backend Backend
	for _, bname := range repo.Backends {
		repo.msg.Infof("checking availability of backend [%s]\n", bname)
		ba, err := NewBackend(bname, repo)
		if err != nil {
			continue
		}
		_ /*repomd*/, ok := md[ba.YumDataType()]
		if !ok {
			repo.msg.Warnf("local repository does not provide [%s] DB\n", bname)
			continue
		}

		// a priori a match
		backend = ba
		repo.Backend = backend

		// loading data necessary for the backend
		err = repo.Backend.LoadDB()
		if err != nil {
			repo.msg.Warnf("problem loading data for backend [%s]: %v\n", bname, err)
			err = nil
			backend = nil
			repo.Backend = nil
			continue
		}

		// stop at first one found.
		break
	}

	if backend == nil {
		repo.msg.Errorf("No valid backend found\n")
		return fmt.Errorf("No valid backend found")
	}

	repo.msg.Infof("repository [%s] - chosen backend [%T]\n", repo.Name, repo.Backend)
	return err
}

// remoteMetadata retrieves the repo metadata file content
func (repo *Repository) remoteMetadata() ([]byte, error) {
	resp, err := http.Get(repo.RepoMdUrl)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, resp.Body)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf.Bytes(), err
}

// localMetadata retrieves the repo metadata from the repomd file
func (repo *Repository) localMetadata() ([]byte, error) {
	if !path_exists(repo.LocalRepoMdXml) {
		return nil, nil
	}
	f, err := os.Open(repo.LocalRepoMdXml)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, f)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf.Bytes(), err
}

// checkRepoMD parses the Repository metadata XML content
func (repo *Repository) checkRepoMD(data []byte) (map[string]RepoMD, error) {

	if len(data) <= 0 {
		return nil, nil
	}

	type xmlTree struct {
		XMLName xml.Name `xml:"repomd"`
		Data    []struct {
			Type     string `xml:"type,attr"`
			Checksum string `xml:"checksum"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
			Timestamp float64 `xml:"timestamp"`
		}
	}

	var tree xmlTree
	err := xml.Unmarshal(data, &tree)
	if err != nil {
		return nil, err
	}

	db := make(map[string]RepoMD)
	for _, data := range tree.Data {
		sec := int64(math.Floor(data.Timestamp))
		nsec := int64((data.Timestamp - float64(sec)) * 1e9)
		db[data.Type] = RepoMD{
			Checksum:  data.Checksum,
			Timestamp: time.Unix(sec, nsec),
			Location:  data.Location.Href,
		}
		repo.msg.Infof(">>> %s: %v\n", data.Type, db[data.Type])
	}
	return db, err
}

type RepoMD struct {
	Checksum  string
	Timestamp time.Time
	Location  string
}

// EOF
