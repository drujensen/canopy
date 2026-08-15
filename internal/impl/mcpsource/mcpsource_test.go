package mcpsource

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeMCPFile(t *testing.T, projectRoot, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(projectRoot, ".mcp.json"), []byte(content), 0o644))
}

func TestLoad_HappyPath(t *testing.T) {
	projectRoot := t.TempDir()

	writeMCPFile(t, projectRoot, `{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
      "env": {"DEBUG": "true"}
    },
    "remote-http": {
      "type": "http",
      "url": "https://example.com/mcp"
    },
    "remote-sse": {
      "type": "sse",
      "url": "https://example.com/mcp-sse"
    }
  }
}`)

	servers, err := Load(projectRoot)
	require.NoError(t, err)
	require.Len(t, servers, 3)

	fs := servers["filesystem"]
	assert.Equal(t, KindStdio, fs.Kind)
	assert.Equal(t, "npx", fs.Command)
	assert.Equal(t, []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"}, fs.Args)
	assert.Equal(t, map[string]string{"DEBUG": "true"}, fs.Env)

	httpSrv := servers["remote-http"]
	assert.Equal(t, KindHTTP, httpSrv.Kind)
	assert.Equal(t, "https://example.com/mcp", httpSrv.URL)

	sseSrv := servers["remote-sse"]
	assert.Equal(t, KindSSE, sseSrv.Kind)
	assert.Equal(t, "https://example.com/mcp-sse", sseSrv.URL)
}

func TestLoad_MissingFile(t *testing.T) {
	projectRoot := t.TempDir()

	servers, err := Load(projectRoot)
	require.NoError(t, err)
	assert.Empty(t, servers)
}

func TestLoad_MalformedJSON(t *testing.T) {
	projectRoot := t.TempDir()
	writeMCPFile(t, projectRoot, `{"mcpServers": { not valid json`)

	_, err := Load(projectRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".mcp.json")
}

func TestLoad_MalformedEntry_NeitherShape(t *testing.T) {
	projectRoot := t.TempDir()
	writeMCPFile(t, projectRoot, `{
  "mcpServers": {
    "broken": {}
  }
}`)

	_, err := Load(projectRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broken")
}

func TestLoad_MalformedEntry_UnknownType(t *testing.T) {
	projectRoot := t.TempDir()
	writeMCPFile(t, projectRoot, `{
  "mcpServers": {
    "weird": {"type": "carrier-pigeon", "url": "https://example.com"}
  }
}`)

	_, err := Load(projectRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "weird")
}

func TestLoad_MalformedEntry_RemoteMissingURL(t *testing.T) {
	projectRoot := t.TempDir()
	writeMCPFile(t, projectRoot, `{
  "mcpServers": {
    "no-url": {"type": "http"}
  }
}`)

	_, err := Load(projectRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-url")
}
