package feishu

import "github.com/chenhg5/cc-connect/core"

var _ core.PreviewCleaner = (*Platform)(nil)
var _ core.PreviewHandleFinishPreference = (*Platform)(nil)
var _ core.PreviewFinalizer = (*Platform)(nil)
