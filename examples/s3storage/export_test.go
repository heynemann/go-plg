package s3storage

// SetPageSize sets how many objects each listing request asks for, so that tests reach a listing's later pages
// without uploading thousands of objects.
func SetPageSize(s *Storage, n int32) {
	s.pageSize = n
}
