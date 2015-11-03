package corehttp

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	gopath "path"
	"strings"
	"time"

	humanize "github.com/ipfs/go-ipfs/Godeps/_workspace/src/github.com/dustin/go-humanize"
	"github.com/ipfs/go-ipfs/Godeps/_workspace/src/golang.org/x/net/context"

	key "github.com/ipfs/go-ipfs/blocks/key"
	core "github.com/ipfs/go-ipfs/core"
	"github.com/ipfs/go-ipfs/importer"
	chunk "github.com/ipfs/go-ipfs/importer/chunk"
	dag "github.com/ipfs/go-ipfs/merkledag"
	dagutils "github.com/ipfs/go-ipfs/merkledag/utils"
	path "github.com/ipfs/go-ipfs/path"
	"github.com/ipfs/go-ipfs/routing"
	uio "github.com/ipfs/go-ipfs/unixfs/io"
)

const (
	ipfsPathPrefix = "/ipfs/"
	ipnsPathPrefix = "/ipns/"
)

// gatewayHandler is a HTTP handler that serves IPFS objects (accessible by default at /ipfs/<path>)
// (it serves requests like GET /ipfs/QmVRzPKPzNtSrEzBFm2UZfxmPAgnaLke4DMcerbsGGSaFe/link)
type gatewayHandler struct {
	node   *core.IpfsNode
	config GatewayConfig
}

func newGatewayHandler(node *core.IpfsNode, conf GatewayConfig) (*gatewayHandler, error) {
	i := &gatewayHandler{
		node:   node,
		config: conf,
	}
	return i, nil
}

// TODO(cryptix):  find these helpers somewhere else
func (i *gatewayHandler) newDagFromReader(r io.Reader) (*dag.Node, error) {
	// TODO(cryptix): change and remove this helper once PR1136 is merged
	// return ufs.AddFromReader(i.node, r.Body)
	return importer.BuildDagFromReader(
		i.node.DAG,
		chunk.DefaultSplitter(r),
		importer.BasicPinnerCB(i.node.Pinning.GetManual()))
}

// TODO(btc): break this apart into separate handlers using a more expressive muxer
func (i *gatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(i.node.Context())
	defer cancel()

	if i.config.Writable {
		switch r.Method {
		case "POST":
			i.postHandler(ctx, w, r)
			return
		case "PUT":
			// TODO(cryptix): where are the docs?
			http.Error(w, "writableGateway: PUT method not meaningful on IPFS - use POST and see the docs", http.StatusMethodNotAllowed)
			return
		case "DELETE":
			i.deleteHandler(ctx, w, r)
			return
		}
	}

	if r.Method == "GET" || r.Method == "HEAD" {
		i.getOrHeadHandler(w, r)
		return
	}

	errmsg := "Method " + r.Method + " not allowed: "
	if !i.config.Writable {
		w.WriteHeader(http.StatusMethodNotAllowed)
		errmsg = errmsg + "read only access"
	} else {
		w.WriteHeader(http.StatusBadRequest)
		errmsg = errmsg + "bad request for " + r.URL.Path
	}
	fmt.Fprint(w, errmsg)
	log.Error(errmsg) // TODO(cryptix): log errors until we have a better way to expose these (counter metrics maybe)
}

func (i *gatewayHandler) getOrHeadHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(i.node.Context())
	defer cancel()

	urlPath := r.URL.Path

	// IPNSHostnameOption might have constructed an IPNS path using the Host header.
	// In this case, we need the original path for constructing redirects
	// and links that match the requested URL.
	// For example, http://example.net would become /ipns/example.net, and
	// the redirects and links would end up as http://example.net/ipns/example.net
	originalURLPath := urlPath
	ipnsHostname := false
	hdr := r.Header["X-IPNS-Original-Path"]
	if len(hdr) > 0 {
		originalURLPath = hdr[0]
		ipnsHostname = true
	}

	if i.config.BlockList != nil && i.config.BlockList.ShouldBlock(urlPath) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("403 - Forbidden"))
		return
	}

	nd, err := core.Resolve(ctx, i.node, path.Path(urlPath))
	if err != nil {
		webError(w, "Path Resolve error", err, http.StatusBadRequest)
		return
	}

	etag := gopath.Base(urlPath)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	i.addUserHeaders(w) // ok, _now_ write user's headers.
	w.Header().Set("X-IPFS-Path", urlPath)

	// Suborigin header, sandboxes apps from each other in the browser (even
	// though they are served from the same gateway domain).
	//
	// Omited if the path was treated by IPNSHostnameOption(), for example
	// a request for http://example.net/ would be changed to /ipns/example.net/,
	// which would turn into an incorrect Suborigin: example.net header.
	//
	// NOTE: This is not yet widely supported by browsers.
	if !ipnsHostname {
		pathRoot := strings.SplitN(urlPath, "/", 4)[2]
		w.Header().Set("Suborigin", pathRoot)
	}

	dr, err := uio.NewDagReader(ctx, nd, i.node.DAG)
	if err != nil && err != uio.ErrIsDir {
		// not a directory and still an error
		internalWebError(w, err)
		return
	}

	// set these headers _after_ the error, for we may just not have it
	// and dont want the client to cache a 500 response...
	// and only if it's /ipfs!
	// TODO: break this out when we split /ipfs /ipns routes.
	modtime := time.Now()
	if strings.HasPrefix(urlPath, ipfsPathPrefix) {
		w.Header().Set("Etag", etag)
		w.Header().Set("Cache-Control", "public, max-age=29030400")

		// set modtime to a really long time ago, since files are immutable and should stay cached
		modtime = time.Unix(1, 0)
	}

	if err == nil {
		defer dr.Close()
		_, name := gopath.Split(urlPath)
		http.ServeContent(w, r, name, modtime, dr)
		return
	}

	// storage for directory listing
	var dirListing []directoryItem
	// loop through files
	foundIndex := false
	for _, link := range nd.Links {
		if link.Name == "index.html" {
			log.Debugf("found index.html link for %s", urlPath)
			foundIndex = true

			if urlPath[len(urlPath)-1] != '/' {
				// See comment above where originalURLPath is declared.
				http.Redirect(w, r, originalURLPath+"/", 302)
				log.Debugf("redirect to %s", originalURLPath+"/")
				return
			}

			// return index page instead.
			nd, err := core.Resolve(ctx, i.node, path.Path(urlPath+"/index.html"))
			if err != nil {
				internalWebError(w, err)
				return
			}
			dr, err := uio.NewDagReader(ctx, nd, i.node.DAG)
			if err != nil {
				internalWebError(w, err)
				return
			}
			defer dr.Close()

			// write to request
			if r.Method != "HEAD" {
				io.Copy(w, dr)
			}
			break
		}

		// See comment above where originalURLPath is declared.
		di := directoryItem{humanize.Bytes(link.Size), link.Name, gopath.Join(originalURLPath, link.Name)}
		dirListing = append(dirListing, di)
	}

	if !foundIndex {
		if r.Method != "HEAD" {
			// construct the correct back link
			// https://github.com/ipfs/go-ipfs/issues/1365
			var backLink = urlPath

			// don't go further up than /ipfs/$hash/
			pathSplit := strings.Split(backLink, "/")
			switch {
			// keep backlink
			case len(pathSplit) == 3: // url: /ipfs/$hash

			// keep backlink
			case len(pathSplit) == 4 && pathSplit[3] == "": // url: /ipfs/$hash/

			// add the correct link depending on wether the path ends with a slash
			default:
				if strings.HasSuffix(backLink, "/") {
					backLink += "./.."
				} else {
					backLink += "/.."
				}
			}

			// strip /ipfs/$hash from backlink if IPNSHostnameOption touched the path.
			if ipnsHostname {
				backLink = "/"
				if len(pathSplit) > 5 {
					// also strip the trailing segment, because it's a backlink
					backLinkParts := pathSplit[3 : len(pathSplit)-2]
					backLink += strings.Join(backLinkParts, "/") + "/"
				}
			}

			// See comment above where originalURLPath is declared.
			tplData := listingTemplateData{
				Listing:  dirListing,
				Path:     originalURLPath,
				BackLink: backLink,
			}
			err := listingTemplate.Execute(w, tplData)
			if err != nil {
				internalWebError(w, err)
				return
			}
		}
	}
}

func (i *gatewayHandler) postHandler(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	rootPath, err := path.ParsePath(r.URL.Path)
	if err != nil {
		webError(w, "postHandler: ipfs path not valid", err, http.StatusBadRequest)
		return
	}

	rsegs := rootPath.Segments()
	if rsegs[0] == ipnsPathPrefix {
		webError(w, "postHandler: updating named entries not supported", ErrIPNSNotSupported, http.StatusBadRequest)
		return
	}

	var newnode *dag.Node
	if rsegs[len(rsegs)-1] == "QmUNLLsPACCz1vLxQVkXqqLX5R1X345qqfHbsf67hvA3Nn" {
		newnode = uio.NewEmptyDirectory()
	} else {
		putNode, err := i.newDagFromReader(r.Body)
		if err != nil {
			webError(w, "postHandler: Could not create DAG from request", err, http.StatusInternalServerError)
			return
		}
		newnode = putNode
	}

	var newPath string
	if len(rsegs) > 1 {
		newPath = gopath.Join(rsegs[2:]...)
	}

	var newkey key.Key
	rnode, err := core.Resolve(ctx, i.node, rootPath)
	switch ev := err.(type) {
	case path.ErrNoLink:
		// ev.Node < node where resolve failed
		// ev.Name < new link
		// but we need to patch from the root
		rnode, err := i.node.DAG.Get(ctx, key.B58KeyDecode(rsegs[1]))
		if err != nil {
			webError(w, "postHandler: Could not create DAG from request", err, http.StatusInternalServerError)
			return
		}

		e := dagutils.NewDagEditor(i.node.DAG, rnode)
		err = e.InsertNodeAtPath(ctx, newPath, newnode, uio.NewEmptyDirectory)
		if err != nil {
			webError(w, "postHandler: InsertNodeAtPath failed", err, http.StatusInternalServerError)
			return
		}

		newkey, err = e.GetNode().Key()
		if err != nil {
			webError(w, "postHandler: could not get key of edited node", err, http.StatusInternalServerError)
			return
		}

	case nil:
		// object set-data case
		rnode.Data = newnode.Data

		newkey, err = i.node.DAG.Add(rnode)
		if err != nil {
			nnk, _ := newnode.Key()
			rk, _ := rnode.Key()
			webError(w, fmt.Sprintf("postHandler: Could not add newnode(%q) to root(%q)", nnk.B58String(), rk.B58String()), err, http.StatusInternalServerError)
			return
		}
	default:
		log.Warningf("postHandler: unhandled resolve error %T", ev)
		webError(w, "could not resolve root DAG", ev, http.StatusInternalServerError)
		return
	}

	i.addUserHeaders(w) // ok, _now_ write user's headers.
	w.Header().Set("IPFS-Hash", newkey.String())
	http.Redirect(w, r, gopath.Join(ipfsPathPrefix, newkey.String(), newPath), http.StatusCreated)
}

var (
	ErrIPNSNotSupported        = errors.New("writableGateway: /ipns/ not supported")
	ErrNotMeaningfulOnWritable = errors.New("writableGateway: non-meaningful request")
)

func (i *gatewayHandler) deleteHandler(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ipfs" || r.URL.Path == ipfsPathPrefix {
		webErrorWithCode(w, "HTTP DELETE of /ipfs is not meaningful", ErrNotMeaningfulOnWritable, http.StatusMethodNotAllowed)
		return
	}

	rootPath, err := path.ParsePath(r.URL.Path)
	if err != nil {
		webError(w, "deleteHandler: ipfs path not valid", err, http.StatusBadRequest)
		return
	}

	rsegs := rootPath.Segments()
	if rsegs[0] == ipnsPathPrefix {
		webError(w, "deleteHandler: updating named entries not supported", ErrIPNSNotSupported, http.StatusBadRequest)
		return
	}

	if len(rsegs) == 2 { // /ipfs/$hash
		webErrorWithCode(w, "HTTP DELETE of /ipfs/$hash is not meaningful", ErrNotMeaningfulOnWritable, http.StatusMethodNotAllowed)
	}

	rnode, err := i.node.DAG.Get(ctx, key.B58KeyDecode(rsegs[1]))
	if err != nil {
		webError(w, "deleteHandler: Could not create DAG from request", err, http.StatusInternalServerError)
		return
	}

	newPath := gopath.Join(rsegs[2:]...)

	e := dagutils.NewDagEditor(i.node.DAG, rnode)
	if err := e.RmLink(ctx, newPath); err != nil {
		webError(w, "deleteHandler: dag editor failed to rmLink()", err, http.StatusInternalServerError)
		return
	}

	newkey, err := e.GetNode().Key()
	if err != nil {
		webError(w, "deleteHandler: could not get key of edited node", err, http.StatusInternalServerError)
		return
	}

	i.addUserHeaders(w) // ok, _now_ write user's headers.
	w.Header().Set("IPFS-Hash", newkey.String())
	http.Redirect(w, r, gopath.Join(ipfsPathPrefix, newkey.String(), newPath), http.StatusCreated)
}

func (i *gatewayHandler) addUserHeaders(w http.ResponseWriter) {
	for k, v := range i.config.Headers {
		w.Header()[k] = v
	}
}

func webError(w http.ResponseWriter, message string, err error, defaultCode int) {
	if _, ok := err.(path.ErrNoLink); ok {
		webErrorWithCode(w, message, err, http.StatusNotFound)
	} else if err == routing.ErrNotFound {
		webErrorWithCode(w, message, err, http.StatusNotFound)
	} else if err == context.DeadlineExceeded {
		webErrorWithCode(w, message, err, http.StatusRequestTimeout)
	} else {
		webErrorWithCode(w, message, err, defaultCode)
	}
}

func webErrorWithCode(w http.ResponseWriter, message string, err error, code int) {
	w.WriteHeader(code)
	s := fmt.Sprintf("%s: %s", message, err)
	log.Errorf(s) // TODO(cryptix): log errors until we have a better way to expose these (counter metrics maybe)
	fmt.Fprint(w, s)
}

// return a 500 error and log
func internalWebError(w http.ResponseWriter, err error) {
	webErrorWithCode(w, "internalWebError", err, http.StatusInternalServerError)
}
