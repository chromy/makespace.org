// Turns the plain file input on a submission form into a Dropzone drop area.
//
// Dropzone here is a picker, not an uploader: whatever it collects is copied
// into the form's own <input type="file">, and the form posts normally. The
// input is kept in sync as files are added and removed rather than at submit
// time, so there is no question of whether the browser has already serialised
// the form when we write to it.

Dropzone.autoDiscover = false;

document.querySelectorAll("form[data-dropzone]").forEach(function (form) {
  const input = form.querySelector('input[type="file"]');
  if (!input || typeof DataTransfer === "undefined") {
    return; // Leave the plain input alone rather than breaking it.
  }

  const area = document.createElement("div");
  area.className = "dropzone";
  input.insertAdjacentElement("afterend", area);

  // Hidden, not removed: it is still the field that carries the files. A
  // hidden input cannot be `required` — the browser refuses to submit a form it
  // cannot focus the invalid field of — so the server stays the thing that
  // insists on at least one photo.
  input.hidden = true;
  input.removeAttribute("required");

  const dz = new Dropzone(area, {
    url: form.action, // never used; Dropzone insists on one
    autoProcessQueue: false,
    addRemoveLinks: true,
    acceptedFiles: input.accept || undefined,
    maxFiles: Number(form.dataset.maxPhotos) || undefined,
    maxFilesize: Number(form.dataset.maxPhotoMb) || undefined,
    dictDefaultMessage: "Drop photos here, or click to choose",
  });

  // Our own list rather than dz.getAcceptedFiles(): "addedfile" fires *before*
  // Dropzone decides whether it accepts the file, so at that moment the
  // accepted list is still empty and the input would never be filled.
  let chosen = [];

  function syncFiles() {
    const transfer = new DataTransfer();
    chosen.forEach(function (file) {
      transfer.items.add(file);
    });
    input.files = transfer.files;
  }

  dz.on("addedfile", function (file) {
    chosen.push(file);
    syncFiles();
  });

  // Both removal and rejection — too large, wrong type, too many — take the
  // file back out again.
  function forget(file) {
    chosen = chosen.filter(function (f) {
      return f !== file;
    });
    syncFiles();
  }

  dz.on("removedfile", forget);
  dz.on("error", forget);
});
