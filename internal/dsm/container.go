package dsm

import (
	"encoding/json"
	"strconv"
)

// ContainerSettings exports a container's settings as the UI's "Export container settings"
// does: a JSON profile, which includes its environment variables. Treat the file as
// holding secrets.
func (c *Client) ContainerSettings(name string) ([]byte, string, error) {
	return c.Download("SYNO.Docker.Container.Profile", "export", 1, Params{"name": JS(name)})
}

// ContainerCreate makes a standalone container from a profile, as the UI's wizard does:
// the profile is the JSON that ContainerSettings exports. runNow starts it at once.
func (c *Client) ContainerCreate(profile []byte, runNow bool) error {
	_, err := c.Call("SYNO.Docker.Container", "create", 1, Params{
		"is_run_instantly": strconv.FormatBool(runNow), "profile": string(profile)})
	return err
}

// ContainerUpdate applies a profile to a container, as the UI's settings dialog does. The
// profile can rename it (its name field), in which case edit_name is the new name.
func (c *Client) ContainerUpdate(name string, profile []byte) error {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(profile, &p); err != nil {
		return Errorf("profile: %v", err)
	}
	_, err := c.Call("SYNO.Docker.Container", "set", 1, Params{
		"name": JS(name), "edit_name": JS(p.Name), "profile": string(profile)})
	return err
}

// ValidateProfile checks what a profile must have before it's sent: a name and an image.
func ValidateProfile(profile []byte) error {
	var p struct {
		Name  string `json:"name"`
		Image string `json:"image"`
	}
	if err := json.Unmarshal(profile, &p); err != nil {
		return Errorf("profile is not JSON: %v", err)
	}
	if p.Name == "" || p.Image == "" {
		return Errorf("profile needs a name and an image")
	}
	return nil
}
