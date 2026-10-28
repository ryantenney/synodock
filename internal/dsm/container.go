package dsm

// ContainerSettings exports a container's settings as the UI's "Export container settings"
// does: a JSON profile, which includes its environment variables. Treat the file as
// holding secrets.
func (c *Client) ContainerSettings(name string) ([]byte, string, error) {
	return c.Download("SYNO.Docker.Container.Profile", "export", 1, Params{"name": JS(name)})
}
