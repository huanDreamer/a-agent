package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	agenttool "github.com/huan/huan-agent/internal/tool"
)

// SaveArtifactToolName is the tool's name. Exported because the console and the
// prompt both have to be able to name it.
const SaveArtifactToolName = "save_artifact"

// SaveArtifactInput is the parameter schema for the "save_artifact" tool.
type SaveArtifactInput struct {
	Title   string `json:"title" jsonschema:"description=Short human title for this artifact, e.g. \"Q3 usage report\". Used as the page title in 产物中心 and as the file name, required"`
	Content string `json:"content" jsonschema:"description=The artifact's full content, exactly as it should be stored and shown, required"`
	Kind    string `json:"kind,omitempty" jsonschema:"description=One of html / document / image (a data URL) / other. Omit to infer it from name, or from the content when neither is given. html is the default"`
	Name    string `json:"name,omitempty" jsonschema:"description=File name to store it under. The extension decides the served type: .html .md .txt .csv .json .xml .png .jpg .gif .webp .pdf. Omit to let kind decide"`
}

// SaveArtifactOutput is what the "save_artifact" tool returns.
type SaveArtifactOutput struct {
	// URL opens the artifact in a browser, relative to the console's origin.
	URL string `json:"url"`
	// Path is its location in the artifact store, for a reader who has shell
	// access to the server.
	Path string `json:"path"`
	// Kind and MIME say what it was stored as, so the model can tell whether its
	// naming worked out.
	Kind  string `json:"kind"`
	MIME  string `json:"mime"`
	Bytes int64  `json:"bytes"`
	// Message is a sentence the model can quote back to the user as-is.
	Message string `json:"message"`
}

// NewSaveArtifactTool returns the Tool that stores a produced resource.
//
// It takes no arguments, and that is the point: where an artifact goes is a
// property of the turn (which session asked, and which deployment is serving),
// not of the tool. buildArtifact refuses in words the model can act on when no
// store is published — see agenttool.ArtifactsFrom.
func NewSaveArtifactTool() (tool.InvokableTool, error) {
	return utils.InferTool(SaveArtifactToolName, saveArtifactDescription(),
		func(ctx context.Context, in SaveArtifactInput) (SaveArtifactOutput, error) {
			return saveArtifact(ctx, in)
		})
}

func saveArtifactDescription() string {
	return "Save a resource the agent produced — an HTML page, a report, a document, a chart, an image — to " +
		"the artifact store, where it is kept on the server and gets a URL that can be opened in a browser. " +
		"Use it for the things that are neither code nor project documentation: a dashboard or demo page you built, " +
		"a summary or report in markdown, a generated chart or screenshot. The user can browse them from the " +
		"conversation's 产物 button and from 统计监控 → 产物中心. " +
		"It returns the URL; quote it to the user when you have saved something. " +
		"A file the user and the agent are working on together belongs in the workspace (write_file), not here — " +
		"this is for what a task produced."
}

// saveArtifact validates the request and hands it to the turn's store.
func saveArtifact(ctx context.Context, in SaveArtifactInput) (SaveArtifactOutput, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return SaveArtifactOutput{}, errors.New("title is required")
	}
	if strings.TrimSpace(in.Content) == "" {
		return SaveArtifactOutput{}, errors.New("content is required")
	}
	kind, err := normalizeArtifactKind(in.Kind)
	if err != nil {
		return SaveArtifactOutput{}, err
	}

	saver, ok := agenttool.ArtifactsFrom(ctx)
	if !ok {
		// The words matter: this is what the model sees on a surface with no
		// artifact store, and it has to be able to tell the user something true
		// rather than retry.
		return SaveArtifactOutput{}, fmt.Errorf(
			"当前部署没有开启产物存储（save_artifact 不可用）：请把内容直接写在回答里，" +
				"或用 write_file 存进工作区")
	}

	res, err := saver.SaveArtifact(ctx, agenttool.ArtifactInput{
		Title:   title,
		Content: strings.NewReader(in.Content),
		Name:    strings.TrimSpace(in.Name),
		Kind:    kind,
		Source:  SaveArtifactToolName,
	})
	if err != nil {
		return SaveArtifactOutput{}, fmt.Errorf("save artifact: %w", err)
	}

	out := SaveArtifactOutput{
		URL:   res.URL,
		Path:  res.Path,
		Kind:  kind,
		MIME:  res.MIME,
		Bytes: res.Bytes,
	}
	if res.URL != "" {
		out.Message = fmt.Sprintf("已保存，可通过 %s 打开（也在该会话的「产物」列表和 产物中心 里）", res.URL)
	} else {
		// No console is serving files — a one-shot run, say. Saying so is better
		// than quoting a path as if it were a link.
		out.Message = fmt.Sprintf("已保存到服务器上的 %s（当前没有可用的访问 URL）", res.Path)
	}
	return out, nil
}

// normalizeArtifactKind checks the model's kind against the vocabulary.
//
// It is validated here (rather than passed through) so a model that invented
// "\"report\"" is told the real options instead of having its word stored and
// silently ignored by every reader.
func normalizeArtifactKind(kind string) (string, error) {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case "":
		return "", nil
	case "html", "document", "image", "other":
		return k, nil
	case "doc", "markdown", "md", "text":
		return "document", nil
	case "page":
		return "html", nil
	case "img", "picture", "photo", "chart", "screenshot":
		return "image", nil
	default:
		return "", fmt.Errorf("kind %q 无法识别：请用 html / document / image / other 之一，或省略由扩展名判断", kind)
	}
}
