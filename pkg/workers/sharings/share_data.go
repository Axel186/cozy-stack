package sharings

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"runtime"
	"strconv"
	"time"

	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/instance"
	"github.com/cozy/cozy-stack/pkg/jobs"
	"github.com/cozy/cozy-stack/pkg/permissions"
	"github.com/cozy/cozy-stack/pkg/vfs"
	"github.com/cozy/cozy-stack/web/files"
	"github.com/cozy/cozy-stack/web/jsonapi"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/labstack/echo"
)

func init() {
	jobs.AddWorker("sharedata", &jobs.WorkerConfig{
		Concurrency: runtime.NumCPU(),
		WorkerFunc:  SendData,
	})
}

// RecipientInfo describes the recipient information
type RecipientInfo struct {
	URL    string
	Scheme string
	Token  string
}

// SendOptions describes the parameters needed to send data
type SendOptions struct {
	DocID      string
	DocType    string
	Type       string
	Recipients []*RecipientInfo
	Path       string
	DocRev     string

	Selector   string
	Values     []string
	sharedRefs []couchdb.DocReference

	fileOpts *fileOptions
}

type fileOptions struct {
	contentlength string
	mime          string
	md5           string
	queries       url.Values
	content       vfs.File
	set           bool // default value is false
}

var (
	// ErrBadFileFormat is used when the given file is not well structured
	ErrBadFileFormat = errors.New("Bad file format")
	//ErrRemoteDocDoesNotExist is used when the remote doc does not exist
	ErrRemoteDocDoesNotExist = errors.New("Remote doc does not exist")
	// ErrBadPermission is used when a given permission is not valid
	ErrBadPermission = errors.New("Invalid permission format")
)

// fillDetailsAndOpenFile will augment the SendOptions structure with the
// details regarding the file to share and open it so that it can be sent.
//
// WARNING: the file descriptor must be closed!
//
// The idea behind this function is to prevent multiple creations of a file
// descriptor, in order to limit I/O to a single opening.
// This function will set the field `set` of the SendOptions structure to `true`
// the first time it is called and thus causing later calls to immediately
// return.
func (opts *SendOptions) fillDetailsAndOpenFile(fs vfs.VFS, fileDoc *vfs.FileDoc) error {
	if opts.fileOpts != nil && opts.fileOpts.set {
		return nil
	}

	fileOpts := &fileOptions{}

	fileOpts.mime = fileDoc.Mime
	fileOpts.contentlength = strconv.FormatInt(fileDoc.ByteSize, 10)
	fileOpts.md5 = base64.StdEncoding.EncodeToString(fileDoc.MD5Sum)

	// Send references for permissions
	var refs string
	if opts.Selector == consts.SelectorReferencedBy {
		sharedRefs := opts.getSharedReferences()
		b, err := json.Marshal(sharedRefs)
		if err != nil {
			return err
		}
		refs = string(b[:])
	}

	fileOpts.queries = url.Values{
		consts.QueryParamType:         {consts.FileType},
		consts.QueryParamName:         {fileDoc.DocName},
		consts.QueryParamExecutable:   {strconv.FormatBool(fileDoc.Executable)},
		consts.QueryParamCreatedAt:    {fileDoc.CreatedAt.Format(time.RFC1123)},
		consts.QueryParamUpdatedAt:    {fileDoc.UpdatedAt.Format(time.RFC1123)},
		consts.QueryParamReferencedBy: []string{refs},
	}

	content, err := fs.OpenFile(fileDoc)
	if err != nil {
		return err
	}
	fileOpts.content = content
	fileOpts.set = true

	opts.fileOpts = fileOpts
	return nil
}

func (opts *SendOptions) closeFile() error {
	if opts.fileOpts != nil && opts.fileOpts.set {
		return opts.fileOpts.content.Close()
	}

	return nil
}

// If the selector is "referenced_by" then the values are of the form
// "doctype/id". To be able to use them we first need to parse them.
func (opts *SendOptions) getSharedReferences() []couchdb.DocReference {
	if opts.sharedRefs == nil && opts.Selector == consts.SelectorReferencedBy {
		opts.sharedRefs = []couchdb.DocReference{}
		for _, ref := range opts.Values {
			parts := strings.Split(ref, permissions.RefSep)
			if len(parts) != 2 {
				continue
			}

			opts.sharedRefs = append(opts.sharedRefs, couchdb.DocReference{
				Type: parts[0],
				ID:   parts[1],
			})
		}
	}

	return opts.sharedRefs
}

// This function extracts only the relevant references: those that concern the
// sharing.
//
// `sharedRefs` is the set of shared references. The result is thus a subset of
// it or all.
func (opts *SendOptions) extractRelevantReferences(refs []couchdb.DocReference) []couchdb.DocReference {
	var res []couchdb.DocReference

	sharedRefs := opts.getSharedReferences()

	for i, ref := range refs {
		match := false
		for _, sharedRef := range sharedRefs {
			if ref.ID == sharedRef.ID {
				match = true
				break
			}
		}

		if match {
			res = append(res, refs[i])
		}
	}

	return res
}

// SendData sends data to all the recipients
func SendData(ctx context.Context, m *jobs.Message) error {
	domain := ctx.Value(jobs.ContextDomainKey).(string)

	opts := &SendOptions{}
	err := m.Unmarshal(&opts)
	if err != nil {
		return err
	}

	ins, err := instance.Get(domain)
	if err != nil {
		return err
	}
	opts.Path = fmt.Sprintf("/sharings/doc/%s/%s", opts.DocType, opts.DocID)

	if opts.DocType == consts.Files {
		dirDoc, fileDoc, err := ins.VFS().DirOrFileByID(opts.DocID)
		if err != nil {
			return err
		}

		if dirDoc != nil {
			opts.Type = consts.DirType
			log.Debugf("[sharings] Sending directory: %#v", dirDoc)
			return SendDir(ins, opts, dirDoc)
		}
		opts.Type = consts.FileType
		log.Debugf("[sharings] Sending file: %v", fileDoc)
		return SendFile(ins, opts, fileDoc)
	}

	log.Debugf("[sharings] Sending JSON (%v): %v", opts.DocType, opts.DocID)
	return SendDoc(ins, opts)
}

// DeleteDoc asks the recipients to delete the shared document which id was
// provided.
func DeleteDoc(opts *SendOptions) error {
	var errFinal error

	for _, recipient := range opts.Recipients {
		doc, err := getDocAtRecipient(nil, opts.DocType, opts.DocID, recipient)
		if err != nil {
			errFinal = multierror.Append(errFinal, fmt.Errorf("Error while trying to get remote doc : %s", err.Error()))
			continue
		}
		rev := doc.M["_rev"].(string)

		_, errSend := request.Req(&request.Options{
			Domain: recipient.URL,
			Scheme: recipient.Scheme,
			Method: http.MethodDelete,
			Path:   opts.Path,
			Headers: request.Headers{
				"Content-Type":  "application/json",
				"Accept":        "application/json",
				"Authorization": "Bearer " + recipient.Token,
			},
			Queries:    url.Values{"rev": {rev}},
			NoResponse: true,
		})
		if errSend != nil {
			errFinal = multierror.Append(errFinal, fmt.Errorf("Error while trying to share data : %s", errSend.Error()))
		}
	}

	return errFinal
}

// SendDoc sends a JSON document to the recipients.
func SendDoc(ins *instance.Instance, opts *SendOptions) error {
	doc := &couchdb.JSONDoc{}
	if err := couchdb.GetDoc(ins, opts.DocType, opts.DocID, doc); err != nil {
		return err
	}

	// A new doc will be created on the recipient side
	delete(doc.M, "_id")
	delete(doc.M, "_rev")

	for _, rec := range opts.Recipients {
		errs := sendDocToRecipient(opts, rec, doc, http.MethodPost)
		if errs != nil {
			ins.Logger().Error("[sharing] An error occurred while trying to send "+
				"a document to a recipient:", errs)
		}
	}

	return nil
}

// UpdateDoc updates a JSON document at each recipient.
func UpdateDoc(ins *instance.Instance, opts *SendOptions) error {
	doc := &couchdb.JSONDoc{}
	if err := couchdb.GetDoc(ins, opts.DocType, opts.DocID, doc); err != nil {
		return err
	}

	for _, rec := range opts.Recipients {
		// A doc update requires to set the doc revision from each recipient
		remoteDoc, err := getDocAtRecipient(doc, opts.DocType, opts.DocID, rec)
		if err != nil {
			ins.Logger().Error("[sharing] An error occurred while trying to get "+
				"remote doc : ", err)
			continue
		}
		// No changes: nothing to do
		if !docHasChanges(doc, remoteDoc) {
			continue
		}
		rev := remoteDoc.M["_rev"].(string)
		doc.SetRev(rev)

		errs := sendDocToRecipient(opts, rec, doc, http.MethodPut)
		if errs != nil {
			ins.Logger().Error("[sharing] An error occurred while trying to send "+
				"an update: ", err)
		}
	}

	return nil
}

func sendDocToRecipient(opts *SendOptions, rec *RecipientInfo, doc *couchdb.JSONDoc, method string) error {
	body, err := request.WriteJSON(doc.M)
	if err != nil {
		return err
	}

	// Send the document to the recipient
	// TODO : handle send failures
	_, err = request.Req(&request.Options{
		Domain: rec.URL,
		Scheme: rec.Scheme,
		Method: method,
		Path:   opts.Path,
		Headers: request.Headers{
			"Content-Type":  "application/json",
			"Accept":        "application/json",
			"Authorization": "Bearer " + rec.Token,
		},
		Body:       body,
		NoResponse: true,
	})

	return err
}

// SendFile sends a binary file to the recipients.
//
// At this step the sharer must specify the destination directory: since we are
// "creating" the file at the recipient we should specify where to put it. For
// now as we are only sharing albums this destination directory always is
// "Shared With Me".
//
// TODO Handle sharing of directories.
func SendFile(ins *instance.Instance, opts *SendOptions, fileDoc *vfs.FileDoc) error {
	err := opts.fillDetailsAndOpenFile(ins.VFS(), fileDoc)
	if err != nil {
		return err
	}
	defer opts.closeFile()

	// Give the SharedWithMeDirID as parent: this is a creation
	opts.fileOpts.queries.Add(consts.QueryParamDirID, consts.SharedWithMeDirID)

	for _, rec := range opts.Recipients {
		err = sendFileToRecipient(opts, rec, http.MethodPost)
		if err != nil {
			ins.Logger().Errorf("[sharing] An error occurred while trying to share "+
				"file %v: %v", fileDoc.DocName, err)
		}
	}

	return nil
}

// SendDir sends a directory to the recipients.
func SendDir(ins *instance.Instance, opts *SendOptions, dirDoc *vfs.DirDoc) error {
	dirTags := strings.Join(dirDoc.Tags, files.TagSeparator)

	parentID, err := getParentDirID(opts, dirDoc.DirID)
	if err != nil {
		return err
	}

	for _, recipient := range opts.Recipients {
		_, errReq := request.Req(&request.Options{
			Domain: recipient.URL,
			Scheme: recipient.Scheme,
			Method: http.MethodPost,
			Path:   opts.Path,
			Headers: request.Headers{
				echo.HeaderContentType:   echo.MIMEApplicationJSON,
				echo.HeaderAuthorization: "Bearer " + recipient.Token,
			},
			Queries: url.Values{
				consts.QueryParamTags: {dirTags},
				consts.QueryParamName: {dirDoc.DocName},
				consts.QueryParamType: {consts.DirType},
				consts.QueryParamCreatedAt: {
					dirDoc.CreatedAt.Format(time.RFC1123)},
				consts.QueryParamUpdatedAt: {
					dirDoc.CreatedAt.Format(time.RFC1123)},
				consts.QueryParamDirID: {parentID},
			},
			NoResponse: true,
		})
		if errReq != nil {
			ins.Logger().Errorf("[sharing] An error occurred while trying to share "+
				"the directory %v: %v", dirDoc.DocName, err)
		}
	}

	return nil
}

// UpdateOrPatchFile updates the file at the recipients.
//
// Depending on the type of update several actions are possible:
// 1. The actual content of the file was modified so we need to upload the new
//    version to the recipients.
//        -> we send the file.
// 2. The event is dectected as a modification but the recipient do not have it,
//    (a GET on the file returns a 404) so we interpret it as a creation: the
//    sharer modified the file so as to share it.
//        -> we send the file.
// 3. The name of the file has changed.
//        -> we change the metadata.
// 4. The references of the file have changed.
//        -> we update the references.
//
// TODO When sharing directories, handle changes on the dirID.
func UpdateOrPatchFile(ins *instance.Instance, opts *SendOptions, fileDoc *vfs.FileDoc) error {
	md5 := base64.StdEncoding.EncodeToString(fileDoc.MD5Sum)
	// A file descriptor can be open in the for loop.
	defer opts.closeFile()

	for _, recipient := range opts.Recipients {
		// Get recipient data
		_, remoteFileDoc, err := getDirOrFileMetadataAtRecipient(opts.DocID,
			recipient)
		if err != nil {
			// Special case for document not found: send document
			if err == ErrRemoteDocDoesNotExist {
				errf := SendFile(ins, opts, fileDoc)
				if errf != nil {
					ins.Logger().Error("[sharing] An error occurred while trying to "+
						"send file: ", errf)
				}
			} else {
				ins.Logger().Errorf("[sharing] Could not get data at %v: %v",
					recipient.URL, err)
			}
			continue
		}

		md5AtRec := base64.StdEncoding.EncodeToString(remoteFileDoc.MD5Sum)
		opts.DocRev = remoteFileDoc.Rev()

		// The MD5 didn't change: this is a PATCH or a reference update.
		if md5 == md5AtRec {
			// Check the metadata did change to do the patch
			if !fileHasChanges(fileDoc, remoteFileDoc) {
				// Special case to deal with ReferencedBy fields
				if opts.Selector == consts.SelectorReferencedBy {
					refs := findNewRefs(opts, fileDoc, remoteFileDoc)
					if refs != nil {
						erru := updateReferencesAtRecipient(http.MethodPost,
							refs, opts, recipient)
						if erru != nil {
							ins.Logger().Error("[sharing] An error occurred "+
								" while trying to update references: ", erru)
						}
					}
				}
				continue
			}

			patch, errp := generateDirOrFilePatch(nil, fileDoc)
			if errp != nil {
				ins.Logger().Errorf("[sharing] Could not generate patch for file %v: %v",
					fileDoc.DocName, errp)
				continue
			}
			errsp := sendPatchToRecipient(patch, opts, recipient, fileDoc.DirID)
			if errsp != nil {
				ins.Logger().Error("[sharing] An error occurred while trying to "+
					"send patch: ", errsp)
			}
			continue
		}
		// The MD5 did change: this is a PUT
		err = opts.fillDetailsAndOpenFile(ins.VFS(), fileDoc)
		if err != nil {
			ins.Logger().Errorf("[sharing] An error occurred while trying "+
				"to open %v: %v", fileDoc.DocName, err)
			continue
		}
		err = sendFileToRecipient(opts, recipient, http.MethodPut)
		if err != nil {
			ins.Logger().Errorf("[sharing] An error occurred while trying to share an "+
				"update of file %v to a recipient: %v", fileDoc.DocName, err)
		}
	}

	return nil
}

// PatchDir updates the metadata of the corresponding directory at each
// recipient's.
func PatchDir(opts *SendOptions, dirDoc *vfs.DirDoc) error {
	var errFinal error

	patch, err := generateDirOrFilePatch(dirDoc, nil)
	if err != nil {
		return err
	}

	for _, rec := range opts.Recipients {
		rev, err := getDirOrFileRevAtRecipient(opts.DocID, rec)
		if err != nil {
			return err
		}
		opts.DocRev = rev
		err = sendPatchToRecipient(patch, opts, rec, dirDoc.DirID)
		if err != nil {
			errFinal = multierror.Append(errFinal,
				fmt.Errorf("Error while trying to send a patch: %s",
					err.Error()))
		}
	}

	return errFinal
}

// RemoveDirOrFileFromSharing tells the recipient to remove the file or
// directory from the specified sharing.
//
// As of now since we only support sharings through ids or "referenced_by"
// selector the only event that could lead to calling this function would be a
// set of "referenced_by" not applying anymore.
//
// TODO Handle sharing of directories
func RemoveDirOrFileFromSharing(opts *SendOptions) error {
	sharedRefs := opts.getSharedReferences()

	for _, recipient := range opts.Recipients {
		errs := updateReferencesAtRecipient(http.MethodDelete, sharedRefs,
			opts, recipient)
		if errs != nil {
			log.Debugf("[sharings] Could not update reference at "+
				"recipient: %v", errs)
		}
	}

	return nil
}

// DeleteDirOrFile asks the recipients to put the file or directory in the
// trash.
func DeleteDirOrFile(opts *SendOptions) error {
	var errFinal error
	for _, recipient := range opts.Recipients {
		rev, err := getDirOrFileRevAtRecipient(opts.DocID, recipient)
		if err != nil {
			errFinal = multierror.Append(errFinal,
				fmt.Errorf("Error while trying to get a revision at %v: %v", recipient.URL, err))
			continue
		}
		opts.DocRev = rev

		_, err = request.Req(&request.Options{
			Domain: recipient.URL,
			Scheme: recipient.Scheme,
			Method: http.MethodDelete,
			Path:   opts.Path,
			Headers: request.Headers{
				echo.HeaderContentType:   echo.MIMEApplicationJSON,
				echo.HeaderAuthorization: "Bearer " + recipient.Token,
			},
			Queries: url.Values{
				consts.QueryParamRev:  {opts.DocRev},
				consts.QueryParamType: {opts.Type},
			},
			NoResponse: true,
		})

		if err != nil {
			errFinal = multierror.Append(errFinal,
				fmt.Errorf("Error while sending request to %v: %v", recipient.URL, err))
		}
	}

	return nil
}

// Send the file to the recipient.
//
// Two scenarii are possible:
// 1. `opts.DocRev` is empty: the recipient should not have the file in his
//    Cozy.
//    If we recieve a "403" error — document update conflict — then that means
//    the file was already shared and we need to update the relevant
//    information.
// 2. `opts.DocRev` is NOT empty: the recipient already has the file and the
//    sharer is updating it.
func sendFileToRecipient(opts *SendOptions, recipient *RecipientInfo, method string) error {
	if !opts.fileOpts.set {
		return errors.New("[sharing] fileOpts were not set")
	}

	if opts.DocRev != "" {
		opts.fileOpts.queries.Add("rev", opts.DocRev)
	}

	_, err := request.Req(&request.Options{
		Domain: recipient.URL,
		Scheme: recipient.Scheme,
		Method: method,
		Path:   opts.Path,
		Headers: request.Headers{
			"Content-Type":   opts.fileOpts.mime,
			"Accept":         "application/vnd.api+json",
			"Content-Length": opts.fileOpts.contentlength,
			"Content-MD5":    opts.fileOpts.md5,
			"Authorization":  "Bearer " + recipient.Token,
		},
		Queries:    opts.fileOpts.queries,
		Body:       opts.fileOpts.content,
		NoResponse: true,
	})

	return err
}

func sendPatchToRecipient(patch *jsonapi.Document, opts *SendOptions, recipient *RecipientInfo, dirID string) error {
	body, err := request.WriteJSON(patch)
	if err != nil {
		return err
	}

	parentID, err := getParentDirID(opts, dirID)
	if err != nil {
		return err
	}

	_, err = request.Req(&request.Options{
		Domain: recipient.URL,
		Scheme: recipient.Scheme,
		Method: http.MethodPatch,
		Path:   opts.Path,
		Headers: request.Headers{
			echo.HeaderContentType:   jsonapi.ContentType,
			echo.HeaderAuthorization: "Bearer " + recipient.Token,
		},
		Queries: url.Values{
			consts.QueryParamRev:   {opts.DocRev},
			consts.QueryParamType:  {opts.Type},
			consts.QueryParamDirID: {parentID},
		},
		Body:       body,
		NoResponse: true,
	})

	return err
}

// Depending on the `method` given this function does two things:
// 1. If it's "POST" it calls the regular routes for adding references to files.
// 2. If it's "DELETE" it calls the sharing handler because, in addition to
//    removing the references, we need to see if the file is still shared and if
//    not we need to trash it.
func updateReferencesAtRecipient(method string, refs []couchdb.DocReference, opts *SendOptions, recipient *RecipientInfo) error {
	data, err := json.Marshal(refs)
	if err != nil {
		return err
	}
	doc := jsonapi.Document{
		Data: (*json.RawMessage)(&data),
	}
	body, err := request.WriteJSON(doc)
	if err != nil {
		return err
	}

	var path string
	if method == http.MethodPost {
		path = fmt.Sprintf("/files/%s/relationships/referenced_by", opts.DocID)
	} else {
		path = fmt.Sprintf("/sharings/files/%s/referenced_by", opts.DocID)
	}

	_, err = request.Req(&request.Options{
		Domain: recipient.URL,
		Scheme: recipient.Scheme,
		Method: method,
		Path:   path,
		Headers: request.Headers{
			echo.HeaderContentType:   jsonapi.ContentType,
			echo.HeaderAuthorization: "Bearer " + recipient.Token,
		},
		Body:       body,
		NoResponse: true,
	})

	return err
}

// getParentDirID returns the id of the parent directory the file should have at
// the recipient.
//
// Two scenarii are possible:
// 1. There is NO selector: the sharing is based on folders/files. If the file
//    we are about to send has its id in the `values` declared in the
//    permissions then it is one of the targets of this sharing. Its
//    `dirID` must be set to `SharedWithMe`. If not then we don't modify it.
// 2. There is a selector: the sharing is not based on folders/files. We change
//    the `dirID` to `SharedWithMe`.
func getParentDirID(opts *SendOptions, dirID string) (parentID string, err error) {
	if opts.Selector == "" {
		if opts.DocID == consts.RootDirID {
			return "", errors.New("/ cannot be shared")
		}

		if isShared(opts.DocID, opts.Values) {
			return consts.SharedWithMeDirID, nil
		}

		return dirID, nil
	}

	return consts.SharedWithMeDirID, nil
}

func isShared(id string, acceptedIDs []string) bool {
	if id == consts.RootDirID {
		return false
	}

	for _, acceptedID := range acceptedIDs {
		if id == acceptedID {
			return true
		}
	}

	return false
}

// Generates a document patch for the given document.
//
// The server expects a jsonapi.Document structure, see:
// http://jsonapi.org/format/#document-structure
// The data part of the jsonapi.Document contains an ObjectMarshalling, see:
// web/jsonapi/data.go:66
func generateDirOrFilePatch(dirDoc *vfs.DirDoc, fileDoc *vfs.FileDoc) (*jsonapi.Document, error) {
	var patch vfs.DocPatch
	var id string
	var rev string

	if dirDoc != nil {
		patch.Name = &dirDoc.DocName
		patch.DirID = &dirDoc.DirID
		patch.Tags = &dirDoc.Tags
		patch.UpdatedAt = &dirDoc.UpdatedAt
		id = dirDoc.ID()
		rev = dirDoc.Rev()
	} else {
		patch.Name = &fileDoc.DocName
		patch.DirID = &fileDoc.DirID
		patch.Tags = &fileDoc.Tags
		patch.UpdatedAt = &fileDoc.UpdatedAt
		id = fileDoc.ID()
		rev = fileDoc.Rev()
	}

	attrs, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}

	obj := &jsonapi.ObjectMarshalling{
		Type:       consts.Files,
		ID:         id,
		Attributes: (*json.RawMessage)(&attrs),
		Meta:       jsonapi.Meta{Rev: rev},
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}

	return &jsonapi.Document{Data: (*json.RawMessage)(&data)}, nil
}

// getDocAtRecipient returns the document at the given recipient.
func getDocAtRecipient(newDoc *couchdb.JSONDoc, doctype, docID string, recInfo *RecipientInfo) (*couchdb.JSONDoc, error) {
	path := fmt.Sprintf("/data/%s/%s", doctype, docID)

	res, err := request.Req(&request.Options{
		Domain: recInfo.URL,
		Scheme: recInfo.Scheme,
		Method: http.MethodGet,
		Path:   path,
		Headers: request.Headers{
			"Content-Type":  "application/json",
			"Accept":        "application/json",
			"Authorization": "Bearer " + recInfo.Token,
		},
	})
	if err != nil {
		return nil, err
	}

	doc := &couchdb.JSONDoc{}
	if err := request.ReadJSON(res.Body, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func getDirOrFileRevAtRecipient(docID string, recipient *RecipientInfo) (string, error) {
	var rev string
	dirDoc, fileDoc, err := getDirOrFileMetadataAtRecipient(docID, recipient)
	if err != nil {
		return "", err
	}
	if dirDoc != nil {
		rev = dirDoc.Rev()
	} else if fileDoc != nil {
		rev = fileDoc.Rev()
	}

	return rev, nil
}

func getDirOrFileMetadataAtRecipient(id string, recInfo *RecipientInfo) (*vfs.DirDoc, *vfs.FileDoc, error) {
	path := fmt.Sprintf("/files/%s", id)

	res, err := request.Req(&request.Options{
		Domain: recInfo.URL,
		Scheme: recInfo.Scheme,
		Method: http.MethodGet,
		Path:   path,
		Headers: request.Headers{
			echo.HeaderContentType:    echo.MIMEApplicationJSON,
			echo.HeaderAcceptEncoding: echo.MIMEApplicationJSON,
			echo.HeaderAuthorization:  "Bearer " + recInfo.Token,
		},
	})
	if err != nil {
		reqErr := err.(*request.Error)
		if reqErr.Title == "Not Found" {
			return nil, nil, ErrRemoteDocDoesNotExist
		}
		return nil, nil, err
	}
	dirOrFileDoc, err := bindDirOrFile(res.Body)
	if err != nil {
		return nil, nil, err
	}
	if dirOrFileDoc == nil {
		return nil, nil, ErrBadFileFormat
	}
	dirDoc, fileDoc := dirOrFileDoc.Refine()
	return dirDoc, fileDoc, nil
}

// filehasChanges checks that the local file do have changes compared to the
// remote one.
// This is done to prevent infinite loops after a PUT/PATCH in master-master:
// we don't propagate the update if they are similar.
func fileHasChanges(newFileDoc, remoteFileDoc *vfs.FileDoc) bool {
	if newFileDoc.Name() != remoteFileDoc.Name() {
		return true
	}
	if !reflect.DeepEqual(newFileDoc.Tags, remoteFileDoc.Tags) {
		return true
	}
	return false
}

// docHasChanges checks that the local doc do have changes compared to the
// remote one.
// This is done to prevent infinite loops after a PUT/PATCH in master-master:
// we don't mitigate the update if they are similar.
func docHasChanges(newDoc *couchdb.JSONDoc, doc *couchdb.JSONDoc) bool {

	// Compare the incoming doc and the existing one without the _id and _rev
	newID := newDoc.M["_id"].(string)
	newRev := newDoc.M["_rev"].(string)
	rev := doc.M["_rev"].(string)
	delete(newDoc.M, "_id")
	delete(newDoc.M, "_rev")
	delete(doc.M, "_id")
	delete(doc.M, "_rev")

	isEqual := reflect.DeepEqual(newDoc.M, doc.M)

	newDoc.M["_id"] = newID
	newDoc.M["_rev"] = newRev
	doc.M["_rev"] = rev

	return !isEqual
}

// findNewRefs returns the references the remote is missing or nil if the remote
// is up to date with the local version of the file.
//
// This function does not deal with removing references or updating the local
// (i.e. if the remote has more references).
func findNewRefs(opts *SendOptions, fileDoc, remoteFileDoc *vfs.FileDoc) []couchdb.DocReference {
	refs := opts.extractRelevantReferences(fileDoc.ReferencedBy)
	remoteRefs := opts.extractRelevantReferences(remoteFileDoc.ReferencedBy)

	if len(refs) > len(remoteRefs) {
		return findMissingRefs(refs, remoteRefs)
	}

	return nil
}

func findMissingRefs(lref, rref []couchdb.DocReference) []couchdb.DocReference {
	var refs []couchdb.DocReference
	for _, lr := range lref {
		hasRef := false
		for _, rr := range rref {
			if rr.ID == lr.ID && rr.Type == lr.Type {
				hasRef = true
			}
		}
		if !hasRef {
			refs = append(refs, lr)
		}
	}
	return refs
}

func bindDirOrFile(body io.Reader) (*vfs.DirOrFileDoc, error) {
	decoder := json.NewDecoder(body)
	var doc *jsonapi.Document
	var dirOrFileDoc *vfs.DirOrFileDoc

	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	if doc.Data == nil {
		return nil, jsonapi.BadJSON()
	}
	var obj *jsonapi.ObjectMarshalling
	if err := json.Unmarshal(*doc.Data, &obj); err != nil {
		return nil, err
	}
	if obj.Attributes != nil {
		if err := json.Unmarshal(*obj.Attributes, &dirOrFileDoc); err != nil {
			return nil, err
		}
	}
	if rel, ok := obj.GetRelationship(consts.SelectorReferencedBy); ok {
		if res, ok := rel.Data.([]interface{}); ok {
			var refs []couchdb.DocReference
			for _, r := range res {
				if m, ok := r.(map[string]interface{}); ok {
					idd, _ := m["id"].(string)
					typ, _ := m["type"].(string)
					ref := couchdb.DocReference{ID: idd, Type: typ}
					refs = append(refs, ref)
				}
			}
			dirOrFileDoc.ReferencedBy = refs
		}
	}
	dirOrFileDoc.SetID(obj.ID)
	dirOrFileDoc.SetRev(obj.Meta.Rev)

	return dirOrFileDoc, nil
}
