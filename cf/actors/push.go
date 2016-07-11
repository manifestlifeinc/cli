package actors

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"

	"github.com/cloudfoundry/cli/cf/api/applicationbits"
	"github.com/cloudfoundry/cli/cf/api/resources"
	"github.com/cloudfoundry/cli/cf/appfiles"
	. "github.com/cloudfoundry/cli/cf/i18n"
	"github.com/cloudfoundry/cli/cf/models"
	"github.com/cloudfoundry/gofileutils/fileutils"
)

const windowsPathPrefix = `\\?\`

//go:generate counterfeiter . PushActor

type PushActor interface {
	UploadApp(appGUID string, zipFile *os.File, presentFiles []resources.AppFileResource) error
	ProcessPath(dirOrZipFile string, f func(string) error) error
	GatherFiles(localFiles []models.AppFileFields, appDir string, uploadDir string) ([]resources.AppFileResource, bool, error)
	ValidateAppParams(apps []models.AppParams) []error
	MapManifestRoute(routeName string, app models.Application) error
}

type PushActorImpl struct {
	appBitsRepo applicationbits.Repository
	appfiles    appfiles.AppFiles
	zipper      appfiles.Zipper
	routeActor  RouteActor
}

func NewPushActor(appBitsRepo applicationbits.Repository, zipper appfiles.Zipper, appfiles appfiles.AppFiles, routeActor RouteActor) PushActor {
	return PushActorImpl{
		appBitsRepo: appBitsRepo,
		appfiles:    appfiles,
		zipper:      zipper,
		routeActor:  routeActor,
	}
}

// ProcessPath takes in a director of app files or a zip file which contains
// the app files. If given a zip file, it will extract the zip to a temporary
// location, call the provided callback with that location, and then clean up
// the location after the callback has been executed.
//
// This was done so that the caller of ProcessPath wouldn't need to know if it
// was a zip file or an app dir that it was given, and the caller would not be
// responsible for cleaning up the temporary directory ProcessPath creates when
// given a zip.
func (actor PushActorImpl) ProcessPath(dirOrZipFile string, f func(string) error) error {
	if !actor.zipper.IsZipFile(dirOrZipFile) {
		appDir, err := filepath.EvalSymlinks(dirOrZipFile)
		if err != nil {
			return err
		}

		if filepath.IsAbs(appDir) {
			err = f(appDir)
			if err != nil {
				return err
			}
		} else {
			var absPath string
			absPath, err = filepath.Abs(appDir)
			if err != nil {
				return err
			}

			err = f(absPath)
			if err != nil {
				return err
			}
		}

		return nil
	}

	tempDir, err := ioutil.TempDir("", "unzipped-app")
	if err != nil {
		return err
	}

	err = actor.zipper.Unzip(dirOrZipFile, tempDir)
	if err != nil {
		return err
	}

	err = f(tempDir)
	if err != nil {
		return err
	}

	err = os.RemoveAll(tempDir)
	if err != nil {
		return err
	}

	return nil
}

func (actor PushActorImpl) GatherFiles(localFiles []models.AppFileFields, appDir string, uploadDir string) ([]resources.AppFileResource, bool, error) {
	appFileResource := []resources.AppFileResource{}
	for _, file := range localFiles {
		appFileResource = append(appFileResource, resources.AppFileResource{
			Path: file.Path,
			Sha1: file.Sha1,
			Size: file.Size,
		})
	}

	remoteFiles, err := actor.appBitsRepo.GetApplicationFiles(appFileResource)
	if err != nil {
		return []resources.AppFileResource{}, false, err
	}

	filesToUpload := make([]models.AppFileFields, len(localFiles), len(localFiles))
	copy(filesToUpload, localFiles)

	for _, remoteFile := range remoteFiles {
		for i, fileToUpload := range filesToUpload {
			if remoteFile.Path == fileToUpload.Path {
				filesToUpload = append(filesToUpload[:i], filesToUpload[i+1:]...)
			}
		}
	}

	err = actor.appfiles.CopyFiles(filesToUpload, appDir, uploadDir)
	if err != nil {
		return []resources.AppFileResource{}, false, err
	}

	_, err = os.Stat(filepath.Join(appDir, ".cfignore"))
	if err == nil {
		err = fileutils.CopyPathToPath(filepath.Join(appDir, ".cfignore"), filepath.Join(uploadDir, ".cfignore"))
		if err != nil {
			return []resources.AppFileResource{}, false, err
		}
	}

	for i := range remoteFiles {
		fullPath, err := filepath.Abs(filepath.Join(appDir, remoteFiles[i].Path))
		if err != nil {
			return []resources.AppFileResource{}, false, err
		}

		if runtime.GOOS == "windows" {
			fullPath = windowsPathPrefix + fullPath
		}
		fileInfo, err := os.Lstat(fullPath)
		if err != nil {
			return []resources.AppFileResource{}, false, err
		}
		fileMode := fileInfo.Mode()

		if runtime.GOOS == "windows" {
			fileMode = fileMode | 0700
		}

		remoteFiles[i].Mode = fmt.Sprintf("%#o", fileMode)
	}

	return remoteFiles, len(filesToUpload) > 0, nil
}

func (actor PushActorImpl) UploadApp(appGUID string, zipFile *os.File, presentFiles []resources.AppFileResource) error {
	return actor.appBitsRepo.UploadBits(appGUID, zipFile, presentFiles)
}

func (actor PushActorImpl) ValidateAppParams(apps []models.AppParams) []error {
	errs := []error{}

	for _, app := range apps {
		appName := app.Name

		if len(app.Routes) > 0 {
			if app.Hosts != nil && len(*app.Hosts) > 0 {
				errs = append(errs, fmt.Errorf(T("application {{.AppName}} must not be configured with both 'routes' and 'hosts'", map[string]interface{}{"AppName": appName})))
			}

			if app.Domains != nil && len(*app.Domains) > 0 {
				errs = append(errs, fmt.Errorf(T("application {{.AppName}} must not be configured with both 'routes' and 'domains'", map[string]interface{}{"AppName": appName})))
			}

			if app.NoHostname {
				errs = append(errs, fmt.Errorf(T("application {{.AppName}} must not be configured with both 'routes' and have 'no-hostname' set to 'true'", map[string]interface{}{"AppName": appName})))
			}
		}
	}

	if len(errs) > 0 {
		return errs
	}

	return nil
}

func (actor PushActorImpl) MapManifestRoute(routeName string, app models.Application) error {
	hostname, domain, err := actor.routeActor.FindDomain(routeName)
	if err != nil {
		return err
	}

	route, err := actor.routeActor.FindOrCreateRoute(hostname, domain, "", false)
	if err != nil {
		return err
	}

	return actor.routeActor.BindRoute(app, route)
}
