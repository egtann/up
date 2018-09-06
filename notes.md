Check Health With Provision ->
	if success: Check Version
	else: Provision

Check Version ->
	if same: return
	else: Update

Update ->
	Check Health
