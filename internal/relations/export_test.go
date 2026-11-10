package relations

func (t *BKTree) Size() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.idIndex)
}
