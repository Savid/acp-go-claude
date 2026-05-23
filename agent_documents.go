package claudeacp

import (
	"context"
	"maps"

	"github.com/coder/acp-go-sdk"
)

func (a *Agent) documentContext(sessionID acp.SessionId) string {
	a.docsMu.Lock()

	documents := a.documents[sessionID]
	if len(documents) == 0 {
		a.docsMu.Unlock()

		return ""
	}

	cloned := make(map[string]documentState, len(documents))
	maps.Copy(cloned, documents)

	focusedURI := a.focusedDocuments[sessionID]
	a.docsMu.Unlock()

	return documentContextText(cloned, focusedURI)
}

func (a *Agent) knownDocumentSessionLocked(sessionID acp.SessionId) bool {
	if _, ok := a.sessions[sessionID]; ok {
		return true
	}

	_, ok := a.nesSessions[sessionID]

	return ok
}

func (a *Agent) knownDocumentSession(sessionID acp.SessionId) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.knownDocumentSessionLocked(sessionID)
}

// UnstableDidChangeDocument applies document edits for prompt and NES context.
func (a *Agent) UnstableDidChangeDocument(
	ctx context.Context,
	params acp.UnstableDidChangeDocumentNotification,
) error {
	if err := params.Validate(); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	a.mu.Lock()
	known := a.knownDocumentSessionLocked(params.SessionId)
	encoding := a.positionEncoding
	a.mu.Unlock()

	if !known {
		return acp.NewInvalidParams(map[string]any{acpFieldSessionID: params.SessionId})
	}

	a.docsMu.Lock()
	defer a.docsMu.Unlock()

	documents := a.documents[params.SessionId]
	if documents == nil {
		documents = make(map[string]documentState)
		a.documents[params.SessionId] = documents
	}

	document, ok := documents[params.Uri]
	if !ok {
		document = documentState{URI: params.Uri}
	}

	text, err := applyDocumentChanges(document.Text, params.ContentChanges, encoding)
	if err != nil {
		return acp.NewInvalidParams(map[string]any{"range": err.Error()})
	}

	if text == document.Text {
		return nil
	}

	document.Text = text
	document.Version = params.Version
	document.Saved = false
	documents[params.Uri] = document

	return nil
}

// UnstableDidCloseDocument removes a document from the session context.
func (a *Agent) UnstableDidCloseDocument(ctx context.Context, params acp.UnstableDidCloseDocumentNotification) error {
	if err := params.Validate(); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if !a.knownDocumentSession(params.SessionId) {
		return acp.NewInvalidParams(map[string]any{acpFieldSessionID: params.SessionId})
	}

	a.docsMu.Lock()
	defer a.docsMu.Unlock()

	delete(a.documents[params.SessionId], params.Uri)

	if a.focusedDocuments[params.SessionId] == params.Uri {
		delete(a.focusedDocuments, params.SessionId)
	}

	return nil
}

// UnstableDidFocusDocument records the focused document for future prompts.
func (a *Agent) UnstableDidFocusDocument(ctx context.Context, params acp.UnstableDidFocusDocumentNotification) error {
	if err := params.Validate(); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if !a.knownDocumentSession(params.SessionId) {
		return acp.NewInvalidParams(map[string]any{acpFieldSessionID: params.SessionId})
	}

	a.docsMu.Lock()
	defer a.docsMu.Unlock()

	if documents := a.documents[params.SessionId]; documents != nil {
		if document, ok := documents[params.Uri]; ok {
			document.Version = params.Version
			documents[params.Uri] = document
		}
	}

	a.focusedDocuments[params.SessionId] = params.Uri

	return nil
}

// UnstableDidOpenDocument stores an editor document for prompt and NES context.
func (a *Agent) UnstableDidOpenDocument(ctx context.Context, params acp.UnstableDidOpenDocumentNotification) error {
	if params.Uri == "" {
		return acp.NewInvalidParams(map[string]any{jsonFieldURI: validationRequired})
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if !a.knownDocumentSession(params.SessionId) {
		return acp.NewInvalidParams(map[string]any{acpFieldSessionID: params.SessionId})
	}

	a.docsMu.Lock()
	defer a.docsMu.Unlock()

	if a.documents[params.SessionId] == nil {
		a.documents[params.SessionId] = make(map[string]documentState)
	}

	a.documents[params.SessionId][params.Uri] = documentState{
		URI:        params.Uri,
		LanguageID: params.LanguageId,
		Text:       params.Text,
		Version:    params.Version,
		Saved:      true,
	}
	a.focusedDocuments[params.SessionId] = params.Uri

	return nil
}

// UnstableDidSaveDocument marks an editor document as saved.
func (a *Agent) UnstableDidSaveDocument(ctx context.Context, params acp.UnstableDidSaveDocumentNotification) error {
	if err := params.Validate(); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if !a.knownDocumentSession(params.SessionId) {
		return acp.NewInvalidParams(map[string]any{acpFieldSessionID: params.SessionId})
	}

	a.docsMu.Lock()
	defer a.docsMu.Unlock()

	if documents := a.documents[params.SessionId]; documents != nil {
		if document, ok := documents[params.Uri]; ok {
			document.Saved = true
			documents[params.Uri] = document
		}
	}

	return nil
}
