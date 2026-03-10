package tui

import "github.com/charmbracelet/bubbles/key"

// GlobalKeys are always active regardless of tab.
type GlobalKeys struct {
	Tab1   key.Binding
	Tab2   key.Binding
	Tab3   key.Binding
	Tab4   key.Binding
	NextTab key.Binding
	PrevTab key.Binding
	Upload key.Binding
	Quit   key.Binding
	Help   key.Binding
}

var GlobalKeyMap = GlobalKeys{
	Tab1:    key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "Network")),
	Tab2:    key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "Transfers")),
	Tab3:    key.NewBinding(key.WithKeys("3"), key.WithHelp("3", "Vault")),
	Tab4:    key.NewBinding(key.WithKeys("4"), key.WithHelp("4", "Diagnostics")),
	NextTab: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "Next tab")),
	PrevTab: key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "Prev tab")),
	Upload:  key.NewBinding(key.WithKeys("ctrl+u"), key.WithHelp("ctrl+u", "Upload")),
	Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "Quit")),
	Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "Help")),
}

// NavKeys are for table/list navigation.
type NavKeys struct {
	Up     key.Binding
	Down   key.Binding
	Top    key.Binding
	Bottom key.Binding
	Enter  key.Binding
	Filter key.Binding
	Esc    key.Binding
}

var NavKeyMap = NavKeys{
	Up:     key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k/↑", "Up")),
	Down:   key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/↓", "Down")),
	Top:    key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "Top")),
	Bottom: key.NewBinding(key.WithKeys("G"), key.WithHelp("G", "Bottom")),
	Enter:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "Select")),
	Filter: key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "Filter")),
	Esc:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "Clear")),
}

// TransferKeys are for the Transfers tab.
type TransferKeys struct {
	Pause  key.Binding
	Resume key.Binding
	Cancel key.Binding
}

var TransferKeyMap = TransferKeys{
	Pause:  key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "Pause")),
	Resume: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "Resume")),
	Cancel: key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "Cancel")),
}

// VaultKeys are for the Vault tab.
type VaultKeys struct {
	Delete     key.Binding
	CycleFilter key.Binding
	SubView1   key.Binding
	SubView2   key.Binding
}

var VaultKeyMap = VaultKeys{
	Delete:     key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "Delete")),
	CycleFilter: key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "Filter")),
	SubView1:   key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "Shares")),
	SubView2:   key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "Downloads")),
}

// NetworkKeys are for the Network tab.
type NetworkKeys struct {
	Download key.Binding
	Search   key.Binding
}

var NetworkKeyMap = NetworkKeys{
	Download: key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "Download")),
	Search:   key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "Search")),
}
