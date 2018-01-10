package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/search"
)

const (
	MINIMUM_UML_SOURCE_LENGTH = 50
)

type Indexer struct {
	GitHubUrl     string
	Content       string
	Renderer      *Renderer
	SyntaxChecker *SyntaxChecker
	ctx           context.Context
}

type Uml struct {
	GitHubUrl    string      `datastore:"gitHubUrl"`
	Source       string      `datastore:"source,noindex"`
	SourceSHA256 string      `datastore:"sourceSHA256"`
	DiagramType  DiagramType `datastore:"diagramType"`
	Svg          string      `datastore:"svg,noindex"`
	PngBase64    string      `datastore:"pngBase64,noindex"`
	Ascii        string      `datastore:"ascii,noindex"`
}

type DiagramType string

type FTSDocument struct {
	Document string `search:"document"`
}

const (
	TypeSequence  DiagramType = "sequence"
	TypeUsecase   DiagramType = "usecase"
	TypeClass     DiagramType = "class"
	TypeActivity  DiagramType = "activity"
	TypeComponent DiagramType = "component"
	TypeState     DiagramType = "state"
	// TypeObject    DiagramType = "object" // object is rarely detected
	TypeUnknwon DiagramType = "__unknown__"
)

func guessDiagramType(source string, result *SyntaxCheckResult) DiagramType {
	switch result.DiagramType {
	case "SEQUENCE":
		return TypeSequence
	case "DESCRIPTION":
		// Both of Usecase and Component diagram's syntax check results are "DESCRIPTION",
		// so distinct them ad hoc
		if strings.Contains(source, "actor") || strings.Contains(source, "usecase") {
			return TypeUsecase
		} else {
			return TypeComponent
		}
	case "CLASS":
		return TypeClass
	case "ACTIVITY":
		return TypeActivity
	case "STATE":
		return TypeState
	default:
		return TypeUnknwon
	}
}

func NewIndexer(ctx context.Context, renderer *Renderer, syntaxChecker *SyntaxChecker, gitHubUrl string, content string) (*Indexer, error) {
	return &Indexer{
		Content:       content,
		Renderer:      renderer,
		SyntaxChecker: syntaxChecker,
		GitHubUrl:     gitHubUrl,
		ctx:           ctx,
	}, nil
}

func (idxr *Indexer) FindSources() []string {
	sources := make([]string, 0)
	content := idxr.Content
	for {
		startIdx := strings.Index(content, "@startuml")
		endIdx := strings.Index(content, "@enduml")
		log.Debugf(idxr.ctx, "length:%d, startIdx:%d, endIdx:%d", len(content), startIdx, endIdx)
		if startIdx == -1 || endIdx == -1 {
			break
		}
		if startIdx < endIdx {
			source := fmt.Sprintf("%s@enduml", content[startIdx:endIdx])
			if len(source) >= MINIMUM_UML_SOURCE_LENGTH {
				sources = append(sources, source)
			}
		}

		content = content[(endIdx + len("@enduml")):]
	}
	return sources
}

func (idxr *Indexer) Process() error {
	ctx := idxr.ctx
	sources := idxr.FindSources()
	renderer := idxr.Renderer
	syntaxChecker := idxr.SyntaxChecker

	fts, err := search.Open("uml_source")
	if err != nil {
		log.Criticalf(ctx, "failed to open FTS: %s", err)
		return err
	}

	// TODO: txn
	// Delete entities
	var oldUmls []Uml
	q := datastore.NewQuery("Uml").Filter("gitHubUrl =", idxr.GitHubUrl)
	keys, err := q.GetAll(ctx, &oldUmls)
	if err != nil {
		log.Criticalf(ctx, "failed to fetch old umls: %v", err)
		return err
	}
	if len(keys) > 0 {
		log.Infof(ctx, "there are old umls found, so delete them: %v", keys)
		if err := datastore.DeleteMulti(ctx, keys); err != nil {
			log.Criticalf(ctx, "failed to delete old umls: %v", err)
			return err
		}

		// Delete index from full-text search
		for _, key := range keys {
			err = fts.Delete(ctx, fmt.Sprintf("%d", key.IntID()))
			if err == nil {
				log.Infof(ctx, "deleted document from FTS: %v", key.IntID())
			} else {
				if err != search.ErrNoSuchDocument {
					log.Criticalf(ctx, "failed to delete document from FTS: %s", err)
					return err
				}
			}
		}
	}

	for _, source := range sources {
		log.Infof(ctx, "process source: %s", source)

		sourceHash = string(sha256.Sum256([]byte(source)))

		result, err := syntaxChecker.CheckSyntax(source)
		if err != nil {
			log.Criticalf(ctx, "failed to check syntax: %s", err)
			return err
		}
		log.Infof(ctx, "syntax check result: %v", result)

		if !result.Valid {
			log.Infof(ctx, "invalid syntax: %s", source)
			continue
		}
		if !result.HasValidDiagram() {
			log.Infof(ctx, "invalid diagram: %s", source)
			continue
		}

		typ := guessDiagramType(source, result)

		svg, err := renderer.RenderSvg(source)
		if err != nil {
			log.Criticalf(ctx, "failed to render svg: %s", err)
			return err
		}

		png, err := renderer.RenderPng(source)
		if err != nil {
			log.Criticalf(ctx, "failed to render png: %s", err)
			return err
		}
		pngBase64 := base64.StdEncoding.EncodeToString(png)

		ascii, err := renderer.RenderAscii(source)
		if err != nil {
			log.Criticalf(ctx, "failed to render ascii: %s", err)
			return err
		}

		log.Infof(ctx, "make index: type=%s, svg=%s, pngBase64=%s, ascii=%s", typ, svg, pngBase64, ascii)
		uml := &Uml{
			GitHubUrl:   idxr.GitHubUrl,
			Source:      source,
			DiagramType: typ,
			Svg:         svg,
			PngBase64:   pngBase64,
			Ascii:       ascii,
		}

		key := datastore.NewIncompleteKey(ctx, "Uml", nil)
		key, err = datastore.Put(ctx, key, uml)
		if err != nil {
			log.Criticalf(ctx, "put error: %s", err)
			return err
		}

		// Register to full-text search index
		doc := FTSDocument{
			Document: source,
		}
		_, err = fts.Put(ctx, fmt.Sprintf("%d", key.IntID()), &doc)
		if err != nil {
			log.Criticalf(ctx, "failed to put document to FTS: %s", err)
			// Ignore error
			continue
		}
	}

	return nil
}
