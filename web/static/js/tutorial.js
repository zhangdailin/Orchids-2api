(function () {
  'use strict';

  var base = window.location.origin.replace(/\/$/, '');

  document.querySelectorAll('[data-api-base]').forEach(function (node) {
    node.textContent = base;
  });

  document.querySelectorAll('[data-api-path]').forEach(function (node) {
    node.textContent = base + (node.getAttribute('data-api-path') || '');
  });

  document.addEventListener('click', function (event) {
    var button = event.target.closest('[data-copy-target]');
    if (!button) { return; }

    var target = document.getElementById(button.getAttribute('data-copy-target'));
    if (target && typeof copyToClipboard === 'function') {
      copyToClipboard(target.textContent || '');
    }
  });
})();
