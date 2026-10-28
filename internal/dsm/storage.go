package dsm

import "strings"

// ShareFreeBytes is the free space on the volume a NAS folder is on, found through its
// shared folder (the first part of the path, as DSM names shares).
func (c *Client) ShareFreeBytes(folder string) (int64, error) {
	first, _, _ := strings.Cut(strings.Trim(folder, "/"), "/")
	share := "/" + first
	var data struct {
		Shares []struct {
			Path       string `json:"path"`
			Additional struct {
				VolumeStatus struct {
					FreeSpace int64 `json:"freespace"`
				} `json:"volume_status"`
			} `json:"additional"`
		} `json:"shares"`
	}
	err := c.CallInto(&data, "SYNO.FileStation.List", "list_share", 2, Params{"additional": JS([]string{"volume_status"})})
	if err != nil {
		return 0, err
	}
	for _, s := range data.Shares {
		if s.Path == share {
			return s.Additional.VolumeStatus.FreeSpace, nil
		}
	}
	return 0, Errorf("no share %s", share)
}
