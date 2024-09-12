package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/acorn-io/baaah/pkg/router"
	"github.com/gptscript-ai/go-gptscript"
	"github.com/gptscript-ai/otto/pkg/storage"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/authentication/user"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Context struct {
	http.ResponseWriter
	*http.Request
	GPTClient *gptscript.GPTScript
	Storage   storage.Client
	User      user.Info
}

func (r *Context) IsStream() bool {
	return slices.Contains(r.Request.Header.Values("Accept"), "text/event-stream")
}

func (r *Context) Read(obj any) error {
	data, err := r.Body()
	if err != nil {
		return err
	}
	return json.Unmarshal(data, obj)
}

func (r *Context) Body() ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Request.Body, 1<<20))
}

func (r *Context) Write(obj any) error {
	if data, ok := obj.([]byte); ok {
		_, err := r.ResponseWriter.Write(data)
		return err
	}
	r.ResponseWriter.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(r.ResponseWriter).Encode(obj)
}

func (r *Context) WriteDataEvent(obj any) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	if _, err = r.ResponseWriter.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err = r.ResponseWriter.Write(data); err != nil {
		return err
	}
	if _, err = r.ResponseWriter.Write([]byte("\n\n")); err != nil {
		return err
	}
	r.Flush()
	return nil
}

func (r *Context) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func Watch[T client.Object](r Context, list client.ObjectList) (<-chan T, error) {
	if err := r.List(list); err != nil {
		return nil, err
	}

	startList := list.DeepCopyObject().(client.ObjectList)

	w, err := r.Storage.Watch(r.Request.Context(), list, &client.ListOptions{
		Namespace: r.Namespace(),
		Raw: &metav1.ListOptions{
			ResourceVersion: list.GetResourceVersion(),
		},
	})
	if err != nil {
		return nil, err
	}

	resp := make(chan T)
	go func() {
		defer close(resp)
		defer w.Stop()

		_ = meta.EachListItem(startList, func(object runtime.Object) error {
			resp <- object.(T)
			return nil
		})

		for event := range w.ResultChan() {
			switch event.Type {
			case watch.Added, watch.Modified, watch.Deleted:
				resp <- event.Object.(T)
			}
		}
	}()

	return resp, nil
}

func (r *Context) List(obj client.ObjectList) error {
	namespace := r.Namespace()
	return r.Storage.List(r.Request.Context(), obj, &client.ListOptions{
		Namespace: namespace,
	})
}

func (r *Context) Delete(obj client.Object) error {
	err := r.Storage.Delete(r.Request.Context(), obj)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (r *Context) Get(obj client.Object, name string) error {
	namespace := r.Namespace()
	err := r.Storage.Get(r.Request.Context(), router.Key(namespace, name), obj)
	if apierrors.IsNotFound(err) {
		gvk, _ := r.Storage.GroupVersionKindFor(obj)
		return NewErrHttp(http.StatusNotFound, fmt.Sprintf("%s %s not found", strings.ToLower(gvk.Kind), name))
	}
	return err
}

func (r *Context) Create(obj client.Object) error {
	return r.Storage.Create(r.Request.Context(), obj)
}

func (r *Context) Update(obj client.Object) error {
	return r.Storage.Update(r.Request.Context(), obj)
}

func (r *Context) Namespace() string {
	if r.User.GetUID() != "" {
		return r.User.GetUID()
	}
	if r.User.GetName() != "" {
		return r.User.GetName()
	}
	return "default"
}
