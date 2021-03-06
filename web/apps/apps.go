// Package apps is the HTTP frontend of the application package. It
// exposes the HTTP api install, update or uninstall applications.
package apps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/cozy/cozy-stack/pkg/apps"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/vfs"
	"github.com/cozy/cozy-stack/web/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/permissions"
	"github.com/labstack/echo"
)

// JSMimeType is the content-type for javascript
const JSMimeType = "application/javascript"

const typeTextEventStream = "text/event-stream"

// installHandler handles all POST /:slug request and tries to install
// or update the application with the given Source.
func installHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	slug := c.Param("slug")
	if err := permissions.AllowInstallApp(c, permissions.POST); err != nil {
		return err
	}
	inst, err := apps.NewInstaller(instance, &apps.InstallerOptions{
		SourceURL: c.QueryParam("Source"),
		Slug:      slug,
	})
	if err != nil {
		return wrapAppsError(err)
	}
	go inst.Install()
	return pollInstaller(c, slug, inst)
}

// updateHandler handles all POST /:slug request and tries to install
// or update the application with the given Source.
func updateHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	slug := c.Param("slug")
	if err := permissions.AllowInstallApp(c, permissions.POST); err != nil {
		return err
	}
	inst, err := apps.NewInstaller(instance, &apps.InstallerOptions{
		Slug: slug,
	})
	if err != nil {
		return wrapAppsError(err)
	}
	go inst.Update()
	return pollInstaller(c, slug, inst)
}

// deleteHandler handles all DELETE /:slug used to delete an application with
// the specified slug.
func deleteHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	slug := c.Param("slug")
	if err := permissions.AllowInstallApp(c, permissions.DELETE); err != nil {
		return err
	}
	inst, err := apps.NewInstaller(instance, &apps.InstallerOptions{Slug: slug})
	if err != nil {
		return wrapAppsError(err)
	}
	man, err := inst.Delete()
	if err != nil {
		return wrapAppsError(err)
	}
	return jsonapi.Data(c, http.StatusOK, man, nil)
}

func pollInstaller(c echo.Context, slug string, inst *apps.Installer) error {
	accept := c.Request().Header.Get("Accept")
	if accept != typeTextEventStream {
		man, _, err := inst.Poll()
		if err != nil {
			return wrapAppsError(err)
		}
		go func() {
			for {
				_, done, err := inst.Poll()
				if err != nil {
					log.Errorf("[apps] %s could not be installed: %v", slug, err)
					break
				}
				if done {
					break
				}
			}
		}()
		return jsonapi.Data(c, http.StatusAccepted, man, nil)
	}

	w := c.Response().Writer
	w.Header().Set("Content-Type", typeTextEventStream)
	w.WriteHeader(200)
	for {
		man, done, err := inst.Poll()
		if err != nil {
			var b []byte
			if b, err = json.Marshal(err.Error()); err == nil {
				writeStream(w, "error", string(b))
			}
			break
		}
		buf := new(bytes.Buffer)
		if err := jsonapi.WriteData(buf, man, nil); err == nil {
			writeStream(w, "state", buf.String())
		}
		if done {
			break
		}
	}
	return nil
}

func writeStream(w http.ResponseWriter, event string, b string) {
	s := fmt.Sprintf("event: %s\r\ndata: %s\r\n\r\n", event, b)
	_, err := w.Write([]byte(s))
	if err != nil {
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// listHandler handles all GET / requests which can be used to list
// installed applications.
func listHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	if err := permissions.AllowWholeType(c, permissions.GET, consts.Apps); err != nil {
		return err
	}

	docs, err := apps.List(instance)
	if err != nil {
		return wrapAppsError(err)
	}

	objs := make([]jsonapi.Object, len(docs))
	for i, d := range docs {
		d.Instance = instance
		objs[i] = jsonapi.Object(d)
	}

	return jsonapi.DataList(c, http.StatusOK, objs, nil)
}

// iconHandler gives the icon of an application
func iconHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	slug := c.Param("slug")
	app, err := apps.GetBySlug(instance, slug)
	if err != nil {
		return err
	}

	if err = permissions.Allow(c, permissions.GET, app); err != nil {
		return err
	}

	filepath := path.Join(vfs.AppsDirName, slug, app.Icon)
	r, err := instance.FS().Open(filepath)
	if err != nil {
		return err
	}
	defer r.Close()
	http.ServeContent(c.Response(), c.Request(), filepath, time.Time{}, r)
	return nil
}

// Routes sets the routing for the apps service
func Routes(router *echo.Group) {
	router.GET("/", listHandler)
	router.POST("/:slug", installHandler)
	router.PUT("/:slug", updateHandler)
	router.DELETE("/:slug", deleteHandler)
	router.GET("/:slug/icon", iconHandler)
}

func wrapAppsError(err error) error {
	switch err {
	case apps.ErrInvalidSlugName:
		return jsonapi.InvalidParameter("slug", err)
	case apps.ErrAlreadyExists:
		return jsonapi.Conflict(err)
	case apps.ErrNotFound:
		return jsonapi.NotFound(err)
	case apps.ErrNotSupportedSource:
		return jsonapi.InvalidParameter("Source", err)
	case apps.ErrManifestNotReachable:
		return jsonapi.NotFound(err)
	case apps.ErrSourceNotReachable:
		return jsonapi.BadRequest(err)
	case apps.ErrBadManifest:
		return jsonapi.BadRequest(err)
	}
	if _, ok := err.(*url.Error); ok {
		return jsonapi.InvalidParameter("Source", err)
	}
	return err
}
