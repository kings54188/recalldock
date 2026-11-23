package sync

import "testing"

func TestMergedProjectExportsAndImportsCanonicalOwnership(t *testing.T) {
	for _, alreadySynced := range []bool{false, true} {
		name := "pending source"
		if alreadySynced {
			name = "acknowledged source"
		}
		t.Run(name, func(t *testing.T) {
			src := newTestStore(t)
			seedStoreForSync(t, src)
			if err := src.EnrollProject("proj-a"); err != nil {
				t.Fatal(err)
			}
			if alreadySynced {
				old := NewCloudWithTransport(src, newFakeCloudTransport(), "proj-a")
				if result, err := old.Export("synthetic", "proj-a"); err != nil || result.IsEmpty {
					t.Fatalf("seed source acknowledgement: %+v %v", result, err)
				}
			}
			if _, err := src.MergeProjects([]string{"proj-a"}, "canonical"); err != nil {
				t.Fatal(err)
			}
			transport := newFakeCloudTransport()
			exporter := NewCloudWithTransport(src, transport, "canonical")
			if result, err := exporter.Export("synthetic", "canonical"); err != nil || result.IsEmpty {
				t.Fatalf("canonical ownership was never exported: %+v %v", result, err)
			}
			dst := newTestStore(t)
			if err := dst.EnrollProject("canonical"); err != nil {
				t.Fatal(err)
			}
			importer := NewCloudWithTransport(dst, transport, "canonical")
			if result, err := importer.Import(); err != nil || result.ChunksImported != 1 {
				t.Fatalf("canonical import: %+v %v", result, err)
			}
			observations, err := dst.RecentObservations("canonical", "", 10)
			if err != nil || len(observations) != 1 || observations[0].Title != "project observation" {
				t.Fatalf("canonical observation missing: %+v %v", observations, err)
			}
			for _, unselected := range []string{"proj-a", "proj-b"} {
				if got, err := dst.RecentObservations(unselected, "", 10); err != nil || len(got) != 0 {
					t.Fatalf("import restored unselected ownership %s: %+v %v", unselected, got, err)
				}
			}
			if result, err := importer.Import(); err != nil || result.ChunksImported != 0 || result.ChunksSkipped != 1 {
				t.Fatalf("canonical import replay: %+v %v", result, err)
			}
			if result, err := exporter.Export("synthetic", "canonical"); err != nil || !result.IsEmpty {
				t.Fatalf("canonical export replay: %+v %v", result, err)
			}
		})
	}
}
