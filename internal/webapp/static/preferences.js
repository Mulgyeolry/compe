(() => {
  "use strict";

  const frequency = document.querySelector("#delivery-frequency");
  const timeField = document.querySelector('[data-delivery-field="time"]');
  const weeklyField = document.querySelector('[data-delivery-field="weekly"]');
  if (!frequency || !timeField || !weeklyField) return;

  const updateDeliveryFields = () => {
    timeField.hidden = frequency.value === "immediate";
    weeklyField.hidden = frequency.value !== "weekly";
    timeField.querySelector("input").disabled = frequency.value === "immediate";
    weeklyField.querySelector("select").disabled = frequency.value !== "weekly";
  };

  frequency.addEventListener("change", updateDeliveryFields);
  updateDeliveryFields();
})();
