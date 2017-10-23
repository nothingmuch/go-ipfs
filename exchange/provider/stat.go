package provider

type Stat struct {
	ProvideBufLen int
}

func (p *provider) Stat() (*Stat, error) {
	return &Stat{
		ProvideBufLen: len(p.newBlocks),
	}, nil
}
