package dsm

import (
	"strconv"
)

// Event is an entry in Container Manager's own log (its Log tab): what it did, and
// when. Container logs are separate; see ContainerLogs.
type Event struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	User  string `json:"user"`
	Event string `json:"event"`
}

// EventPage is one page of the log, newest first, with the counts the UI shows.
type EventPage struct {
	Events     []Event `json:"logs"`
	Total      int     `json:"total"`
	ErrorCount int     `json:"error_count"`
	WarnCount  int     `json:"warn_count"`
	InfoCount  int     `json:"info_count"`
}

// Events is the newest events. level is "" for all, or the UI's names for one: error,
// warning or information; keyword filters the text.
func (c *Client) Events(limit int, level, keyword string) (*EventPage, error) {
	var page EventPage
	err := c.CallInto(&page, "SYNO.Docker.Log", "list", 1, Params{
		"action": JS("load"), "offset": "0", "limit": strconv.Itoa(limit), "sort_by": JS("time"), "sort_dir": JS("DESC"),
		"loglevel": JS(level), "filter_content": JS(keyword), "datefrom": "0", "dateto": "0"})
	return &page, err
}

// EventsClear empties Container Manager's log. There's no undo.
func (c *Client) EventsClear() error {
	_, err := c.Call("SYNO.Docker.Log", "clear", 1, nil)
	return err
}

// EventsExport is the whole log, filtered, as the NAS's own CSV or HTML file. It returns
// the bytes and the file name the NAS gives them.
func (c *Client) EventsExport(format, level, keyword string) ([]byte, string, error) {
	return c.Download("SYNO.Docker.Log", "export", 1, Params{
		"format": JS(format), "loglevel": JS(level), "filter_content": JS(keyword), "datefrom": "0", "dateto": "0"})
}
